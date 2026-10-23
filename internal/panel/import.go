package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 一条待导入凭证（cockpit 导出字段 + 面板 auth 字段合并后的中间形态）。
//
// 下面是 cockpit tools 那套 snake_case 字段；面板/插件的 auth 文件是 camelCase 且嵌在
// auth/account 里，由 decodeCockpitAccount 归一到这里，处理逻辑不必分叉。
type cockpitAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	DosageNotify string `json:"dosage_notify_code"`
	PaymentType  string `json:"payment_type"`
	Status       string `json:"status"`

	// auth 文件形状专有：realm / 企业 id / 设备 token 在 cockpit 导出里没有，
	// 但从别的实例搬 auths/ 时必须带上，否则企业号会被当成个人号走成长任务。
	Realm        string `json:"realm"`
	EnterpriseID string `json:"enterpriseId"`
	DeviceToken  string `json:"device_token"`

	// expiresAtSec 归一后的过期时间（Unix 秒）。cockpit 导出是毫秒、auth 文件是秒，
	// 单位判断放在解码期做一次，导入主流程里不再出现 /1000 这种约定。
	expiresAtSec int64
}

// cockpitWrapperKeys 是「账号数组被包在对象里」时可能出现的字段名，按优先级尝试。
// 注意不放 "account"：那是 auth 文件形状里的账号对象，不是外壳，放进来会把
// 「一个账号但没带 token」误当成「外壳里没账号」。
var cockpitWrapperKeys = []string{
	"accounts", "data", "items", "list", "result", "results",
	"users", "rows", "records", "exported_accounts", "cockpit_accounts",
}

// utf8BOM 是记事本 / Windows 工具导出的常见前缀，json.Unmarshal 不认它。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// decodeCockpitAccount 把一条待导入记录解成 cockpitAccount，认两种实际见过的形状：
//
//  1. 面板/插件 auth 文件：嵌套 {"auth":{accessToken,refreshToken,expiresAt,domain,realm},
//     "account":{uid,enterpriseId,nickname}}，或同字段的扁平 camelCase。cockpit tools 的
//     导出与另一个 workbuddy2api 实例的 auths/*.json 都是这个样子（2026-10-23 现场：
//     用户导的 10 份文件每份 total=1 imported=0 skipped=1，就是只按 snake_case 读、
//     读出来 uid/at/rt 全空被当「缺字段」跳过）。
//  2. cockpit 自带的 snake_case 行（access_token / refresh_token / expires_at 毫秒）。
//
// 形状 1 直接复用 auth.Parse，别在这里手写第二套 JSON 解析——那份解析连 device_token、
// 嵌套形/扁平形都覆盖了，重复实现只会慢慢漂移。
func decodeCockpitAccount(raw []byte) (cockpitAccount, error) {
	var acc cockpitAccount
	fromAuthFile := false

	if a, err := auth.Parse(raw); err == nil && strings.TrimSpace(a.AccessToken) != "" {
		acc.UID = a.UID
		acc.AccessToken = a.AccessToken
		acc.RefreshToken = a.RefreshToken
		acc.Domain = a.Domain
		acc.Nickname = a.Nickname
		acc.EnterpriseID = a.EnterpriseID
		acc.DeviceToken = a.DeviceToken
		acc.Realm = a.RealmStored()
		acc.expiresAtSec = a.ExpiresAt
		fromAuthFile = true
	}

	var ck cockpitAccount
	if err := json.Unmarshal(raw, &ck); err == nil {
		if fromAuthFile {
			// auth 文件里没有的展示性字段（id/email）从 snake_case 补齐；
			// 有值的一律以 auth.Parse 结果为准。
			if acc.UID == "" {
				acc.UID = ck.UID
			}
			if acc.Nickname == "" {
				acc.Nickname = ck.Nickname
			}
			if acc.Domain == "" {
				acc.Domain = ck.Domain
			}
			acc.Email = ck.Email
			acc.ID = ck.ID
			if acc.expiresAtSec <= 0 {
				acc.expiresAtSec = normalizeExpiry(ck.ExpiresAt)
			}
		} else {
			acc = ck
			acc.expiresAtSec = normalizeExpiry(ck.ExpiresAt)
		}
	}

	if acc.AccessToken == "" && acc.RefreshToken == "" && acc.UID == "" {
		return acc, errors.New("认不出账号字段（既不是 auth 文件形状，也没有 cockpit 的 access_token/refresh_token/uid）")
	}
	return acc, nil
}

// normalizeExpiry 把过期时间统一成 Unix 秒。cockpit 导出是毫秒（13 位），
// auth 文件是秒（10 位）；阈值取 1e11（≈ 5138 年，远超任何真实秒级时间戳）。
func normalizeExpiry(v int64) int64 {
	switch {
	case v <= 0:
		return 0
	case v > 100_000_000_000:
		return v / 1000
	default:
		return v
	}
}

// parseCockpitAccounts 解析导入文件，兼容这些外壳：
//
//  1. 裸数组：[{...},{...}]（cockpit 经典格式，仍然优先）
//  2. 对象包数组：{"accounts":[...], "exported_at":...}
//  3. 对象套对象：{"data":{"accounts":[...]}}
//  4. 单个账号：auth 文件的嵌套/扁平形态，或一条 cockpit 行
//  5. 以 uid 为键的字典：{"uid-1":{...},"uid-2":{...}}
//  6. 上面任意一种被二次编码成 JSON 字符串，以及带 BOM 的文件
//
// 全部不匹配时，错误里回显顶层字段名与文件开头片段，而不是只丢一句
// "cannot unmarshal object into Go value of type []panel.cockpitAccount"
// ——那种文案对着一份真实文件没法判断到底是哪一层套错了。
func parseCockpitAccounts(raw []byte) ([]cockpitAccount, error) {
	return parseCockpitAccountsDepth(raw, 0)
}

func parseCockpitAccountsDepth(raw []byte, depth int) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, utf8BOM))
	if len(trimmed) == 0 {
		return nil, errors.New("文件是空的")
	}
	if depth > 3 {
		return nil, errors.New("JSON 嵌套过深，找不到账号数组")
	}

	// 1. 数组：逐个元素解码（元素可以是 cockpit 行，也可以是 auth 文件形状）。
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		out := make([]cockpitAccount, 0, len(arr))
		var firstErr error
		for _, el := range arr {
			acc, derr := decodeCockpitAccount(el)
			if derr != nil {
				if firstErr == nil {
					firstErr = derr
				}
				continue
			}
			out = append(out, acc)
		}
		if len(out) == 0 && firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	}

	// 6. 二次编码：整个 JSON 被当成一个字符串塞进 JSON。
	var encoded string
	if err := json.Unmarshal(trimmed, &encoded); err == nil {
		return parseCockpitAccountsDepth([]byte(encoded), depth+1)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("既不是 JSON 数组也不是对象: %v%s", err, rawPreview(trimmed))
	}

	// 4. 单个账号（两条凭证都齐才算，避免把外壳的元数据当账号）。
	if acc, err := decodeCockpitAccount(trimmed); err == nil &&
		acc.AccessToken != "" && acc.RefreshToken != "" {
		return []cockpitAccount{acc}, nil
	}

	// 2/3. 外壳对象：按字段名找（大小写不敏感），找对了就递归下去。
	lowered := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		lowered[strings.ToLower(k)] = v
	}
	var tried []string
	for _, want := range cockpitWrapperKeys {
		sub, ok := lowered[want]
		if !ok {
			continue
		}
		got, err := parseCockpitAccountsDepth(sub, depth+1)
		if err == nil {
			return got, nil // 空数组也算命中，交给上层报「文件里没有账号」
		}
		tried = append(tried, want)
	}

	// 5. 以 uid/id 为键的字典：{"uid-1":{...},"uid-2":{...}}（有的工具这么落盘）。
	// 只有当**每个**值都能解出账号时才认，否则宁可报错也不要瞎猜。
	dict := make([]cockpitAccount, 0, len(obj))
	dictOK := len(obj) > 0
	for _, v := range obj {
		acc, err := decodeCockpitAccount(v)
		if err != nil || (acc.AccessToken == "" && acc.RefreshToken == "") {
			dictOK = false
			break
		}
		dict = append(dict, acc)
	}
	if dictOK {
		sort.Slice(dict, func(i, j int) bool { return dict[i].UID < dict[j].UID })
		return dict, nil
	}

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	msg := fmt.Sprintf("顶层是对象但里面没找到账号数组（现有字段: %s）%s", strings.Join(keys, ", "), rawPreview(trimmed))
	if len(tried) > 0 {
		msg = fmt.Sprintf("字段 %s 里都不是账号数组（顶层字段: %s）%s", strings.Join(tried, ", "), strings.Join(keys, ", "), rawPreview(trimmed))
	}
	return nil, errors.New(msg)
}

// rawPreview 给错误信息附一小段原文，方便人对着文件看是哪层套错了。
func rawPreview(raw []byte) string {
	const max = 120
	if len(raw) > max {
		return "；文件开头: " + string(raw[:max]) + "…"
	}
	return "；文件内容: " + string(raw)
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	accounts, err := parseCockpitAccounts(raw)
	if err != nil {
		log.Printf("panel: cockpit import 解析失败 file=%s bytes=%d: %v", hdr.Filename, len(raw), err)
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		log.Printf("panel: cockpit import 文件里没有账号 file=%s bytes=%d", hdr.Filename, len(raw))
		writeErr(w, http.StatusBadRequest, "文件里没有账号（账号数组为空）")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			// 这条错误文案会直接回到面板上，缺哪个字段必须点名，不然用户只知道「失败」。
			var miss []string
			if uid == "" {
				miss = append(miss, "uid")
			}
			if at == "" {
				miss = append(miss, "accessToken")
			}
			if rt == "" {
				miss = append(miss, "refreshToken")
			}
			skipped++
			errs = append(errs, fmt.Sprintf("缺字段 %s（id=%s，邮件=%s）", strings.Join(miss, "/"), acc.ID, acc.Email))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("uid 含非法字符（id=%s）", acc.ID))
			continue
		}

		// realm：auth 文件里写着就直接用（企业/国际号带过来不能靠域名猜），
		// 否则按 domain 推断（workbuddy.ai 家族 → global，其余 cn）。
		realm := strings.ToLower(strings.TrimSpace(acc.Realm))
		if realm != "cn" && realm != "global" {
			realm = auth.ResolveRealm("", acc.Domain)
		}

		expiresAt := acc.expiresAtSec
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			EnterpriseID: acc.EnterpriseID,
			DeviceToken:  acc.DeviceToken,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
			continue
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		isNewAccount := p.addImportedAccount(a)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}

		if isNewAccount {
			p.StartNewAccountTasks(uid)
		}
		imported++
	}

	total = len(accounts)
	// 跳过原因也落日志：面板弹窗几分钟就关了，事后只能靠这行复盘。
	for _, e := range errs {
		log.Printf("panel: cockpit import 跳过 file=%s: %s", hdr.Filename, e)
	}
	log.Printf("panel: cockpit import finished file=%s total=%d imported=%d skipped=%d", hdr.Filename, total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// addImportedAccount 把一份导入的凭证放进池子，返回它是否为新号。
//
// 与浏览器登录的分工不同：登录是一个号一个号人工确认，导入是批量灌凭证，来路/健康度
// 都未经验证，故**默认暂停选号**——先把号放进池子（签到 / 活跃 / 保活 / 余额刷新照常
// 跑起来），但不进转发路径；面板看积分、必要时手工试一次，确认没问题再「恢复选号」。
//
// 顺序有讲究：Pool.Revive 是「解冻=全清」语义，会把 paused 一并清掉，所以 Pause 必须
// 排在 Revive 之后，否则暂停会被静默撤销。抽成独立方法也是为了能脱离上游（签到/余额）
// 单测这条语义。
func (p *Panel) addImportedAccount(a *auth.Auth) bool {
	isNew := p.cfg.Pool.AuthByUID(a.UID) == nil
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	p.cfg.Pool.Pause(a.UID)
	return isNew
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
