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

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// cockpitWrapperKeys 是「账号数组被包在对象里」时可能出现的字段名，按优先级尝试。
// cockpit tools 各版本导出外壳不统一：早期是裸数组，后来套过 {"accounts":[...]}、
// {"data":{"accounts":[...]}} 这类带元数据的对象，字段名大小写也不固定。
var cockpitWrapperKeys = []string{
	"accounts", "account", "data", "items", "list", "result", "results",
	"users", "rows", "records", "exported_accounts", "cockpit_accounts",
}

// cockpitCredentialKeys 用来判断一个对象是不是「单个账号」，而不是外壳。
var cockpitCredentialKeys = []string{"access_token", "refresh_token", "uid", "email"}

// utf8BOM 是记事本 / Windows 工具导出的常见前缀，json.Unmarshal 不认它。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// parseCockpitAccounts 解析导入文件，兼容这些外壳：
//
//  1. 裸数组：[{...},{...}]（旧格式，仍然优先）
//  2. 对象包数组：{"accounts":[...], "exported_at":...}
//  3. 对象套对象：{"data":{"accounts":[...]}}
//  4. 单个账号对象：{"uid":..., "access_token":...}（只导一个号时会这么存）
//  5. 上面任意一种被二次编码成 JSON 字符串
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

	// 1. 裸数组：cockpit tools 的经典格式，命中即用。
	var accounts []cockpitAccount
	if err := json.Unmarshal(trimmed, &accounts); err == nil {
		return accounts, nil
	}

	// 5. 二次编码：整个 JSON 被当成一个字符串塞进 JSON。
	var encoded string
	if err := json.Unmarshal(trimmed, &encoded); err == nil {
		return parseCockpitAccountsDepth([]byte(encoded), depth+1)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("既不是 JSON 数组也不是对象: %v%s", err, rawPreview(trimmed))
	}

	// 4. 单个账号对象。
	if looksLikeCockpitAccount(obj) {
		var one cockpitAccount
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, fmt.Errorf("单账号对象解析失败: %v", err)
		}
		return []cockpitAccount{one}, nil
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

	// 6. 以 uid/id 为键的字典：{"uid-1":{...},"uid-2":{...}}（有的工具这么落盘）。
	// 只有当**每个**值都像账号时才认，否则宁可报错也不要瞎猜。
	dict := make([]cockpitAccount, 0, len(obj))
	dictOK := len(obj) > 0
	for _, v := range obj {
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(v, &sub); err != nil || !looksLikeCockpitAccount(sub) {
			dictOK = false
			break
		}
		var one cockpitAccount
		if err := json.Unmarshal(v, &one); err != nil {
			dictOK = false
			break
		}
		dict = append(dict, one)
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

// looksLikeCockpitAccount 判断对象本身是不是一个账号（而不是包账号的外壳）。
func looksLikeCockpitAccount(obj map[string]json.RawMessage) bool {
	for k := range obj {
		lk := strings.ToLower(k)
		for _, want := range cockpitCredentialKeys {
			if lk == want {
				return true
			}
		}
	}
	return false
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
	file, _, err := r.FormFile("file")
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
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
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
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm("", acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
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
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
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
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
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
