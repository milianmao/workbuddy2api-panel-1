package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
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

	// EnterpriseID 兼容导出工具的两种写法：cockpit 字段多为 snake_case，
	// auth 文件与登录接口用 camelCase。任一带值即视为企业版，供签到 /
	// 成长任务门控与额度口径使用（见 auth.IsEnterprise）。
	EnterpriseID      string `json:"enterprise_id"`
	EnterpriseIDCamel string `json:"enterpriseId"`

	// 下面两个只有面板/插件 auth 文件会带，cockpit 导出没有。
	// realm：企业号、国际号从别的实例搬 auths/ 过来时必须跟着走，不能一律按域名猜；
	// device_token：设备指纹的基座，丢了等于换了台设备。
	Realm       string `json:"realm"`
	DeviceToken string `json:"device_token"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>（可重复，按表单顺序逐个解码）
//	      dryRun=true|1 → 只做校验与预览，不落盘、不上游校验、不签到
//
// 多个文件是「整体事务」：任一文件不是合法 JSON 或没有任何账号就整体 400，不做
// 「先把前面导进去」（导入不可逆，半成功最难收拾）。
//
// 返回 {ok, total, imported, skipped, errors, creates, overwrites, dry_run}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	dryRun := strings.EqualFold(strings.TrimSpace(r.FormValue("dryRun")), "true") ||
		strings.TrimSpace(r.FormValue("dryRun")) == "1"

	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}

	var accounts []cockpitAccount
	var sources []string // 与 accounts 平行：每个条目来自哪个文件，出错时能说清
	for _, fh := range headers {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "open "+fh.Filename+": "+err.Error())
			return
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": read file: "+err.Error())
			return
		}

		list, err := decodeCockpitAccounts(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": invalid json: "+err.Error())
			return
		}
		if len(list) == 0 {
			writeErr(w, http.StatusBadRequest, fh.Filename+": empty accounts array")
			return
		}
		for _, acc := range list {
			accounts = append(accounts, acc)
			sources = append(sources, fh.Filename)
		}
	}

	// 凭证目录必须在第一次落盘前就位。login.go / apikey.go 各自在写盘前建了目录，
	// 唯独本文件漏了：全新部署（auths/ 还没生成、也没登录过任何账号）时，写盘用的
	// 临时文件创建直接 ENOENT，用户看到的是「导入失败」，而服务本身完全正常
	// ——LoadDir 用 filepath.Glob，目录不存在时返回空列表且不报错，所以启动日志
	// 一切正常（loaded 0 account(s)），问题只在导入时暴露。
	// 放在循环外建一次：目录是进程级前置条件，逐个账号重复 MkdirAll 既无意义，
	// 也会在部分账号失败时给出误导性错误。预览不建任何东西。
	if !dryRun {
		if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
			return
		}
	}

	var total, imported, skipped, creates, overwrites int
	var errs, warnings []string
	seenUID := map[string]string{} // uid -> 首次出现的文件，用于批内去重
	var toVerify []importAcct      // 池里已有、要覆盖凭据的号：先求证再落盘
	var committed []importAcct     // 已落盘入池的号：循环后做上游动作

	// commit 落盘并入池；落盘在前、入池在后：入池后选号就会用新凭据，磁盘没跟上则重启后回到旧值。
	// verified 表示新凭据刚被 refresh 求证过：上游已把文件里的 refresh token 轮换，池里原有的那份
	// 多半随之作废（旧导出与池里是同一条登录链时），此时落盘失败也必须把新凭据放进内存，
	// 否则这个号就真的只能重新登录了。调用方串行调用（求证回调自己持 mu）。
	commit := func(ia importAcct, verified bool) {
		if err := ia.a.SaveAtomic(); err != nil {
			if !verified {
				skipped++
				errs = append(errs, fmt.Sprintf("%s: save auth failed: %v", ia.src, err))
				return
			}
			log.Printf("WARN: panel: import uid=%s 新凭据已求证但落盘失败，仅放入内存: %v", ia.uid, err)
			warnings = append(warnings, fmt.Sprintf("%s: uid=%s 凭据落盘失败（%v），新令牌只在内存里生效，请检查凭证目录后重新导入一次", ia.src, ia.uid, err))
		}
		// 入池走 addImportedAccount：新号默认 hold（暂停选号，签到/保活/余额照常跑）。
		// 上游这里是裸 Add + Revive，等于导入完立刻进转发路径——我们不这么干。
		p.addImportedAccount(ia.a)
		committed = append(committed, ia)
		imported++
	}
	var mu sync.Mutex // 保护 commit 及其计数：求证回调会并发进来

	for i, acc := range accounts {
		src := sources[i]
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			// 这条文案会直接回到面板上：缺哪个字段必须点名，不然用户只知道「失败」。
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
			// 前半句保留上游原文（上游用例按它断言），后半句点名缺的字段，省得用户只知道「失败」。
			errs = append(errs, fmt.Sprintf("%s: missing required fields (id=%s) — 缺字段 %s", src, acc.ID, strings.Join(miss, "/")))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: invalid uid (id=%s)", src, acc.ID))
			continue
		}
		// 批内去重：多个文件可能有重叠账号。保留第一次出现的那条（选择顺序即
		// 提交顺序，先选的优先），后面的记跳过并说明来源，不静默丢弃。
		if first, ok := seenUID[uid]; ok {
			skipped++
			if first == src {
				errs = append(errs, fmt.Sprintf("%s: duplicate uid=%s in the same file", src, uid))
			} else {
				errs = append(errs, fmt.Sprintf("%s: duplicate uid=%s (already provided by %s)", src, uid, first))
			}
			continue
		}
		seenUID[uid] = src

		// realm：auth 文件里写着就直接用（企业/国际号搬过来不能靠域名猜），
		// 否则按 domain 推断（workbuddy.ai 家族 → global，其余 cn）。
		realm := strings.ToLower(strings.TrimSpace(acc.Realm))
		if realm != "cn" && realm != "global" {
			realm = auth.ResolveRealm("", acc.Domain)
		}

		// expires_at 先统一成毫秒再转秒：批量导出是毫秒，单账号详情导出是秒，
		// 直接除以 1000 会把秒级值算成 1970 年（账号一进池就是「已过期」）。
		expiresAt := normalizeExpiresAtMillis(acc.ExpiresAt) / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}
		enterpriseID := strings.TrimSpace(acc.EnterpriseID)
		if enterpriseID == "" {
			enterpriseID = strings.TrimSpace(acc.EnterpriseIDCamel)
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			EnterpriseID: enterpriseID,
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

		// 覆盖已有凭据是不可逆动作：先用 refresh 求证这份新凭据还活着，只接受
		// 「明确活着」。起因是把一份旧 cockpit 导出重新导入时，文件里的 RT 早已被
		// 轮换，直接覆盖会让本来能用的账号变成要重新登录——.bak 里虽然留着旧凭据，
		// 但没有任何东西会自动拿它回退。判据用 VerifySessionByRefresh 的三态
		// （alive/dead/inconclusive）：dead 是上游明确拒绝，inconclusive 是没判词
		// （网络/5xx/429），后者同样不覆盖——主动销毁时没有判词就不能动手。
		// 顺带一提：alive 分支里 a 已被 refresh 轮换过 RT，下面的落盘存的是新值。
		existing := p.cfg.Pool.AuthByUID(uid)
		if existing == nil {
			creates++
		} else {
			overwrites++
		}
		if dryRun {
			// 预览到此为止，**绝不**往上游发请求：VerifySessionByRefresh 会真的
			// 轮换一次 refresh token（单次有效），只为看一眼就烧掉一个 RT 不行，
			// 更别说它还会顺手改掉内存里的凭据。真实导入时这份校验照旧执行。
			imported++
			continue
		}
		ia := importAcct{a: a, src: src, uid: uid, realm: realm, isNew: existing == nil}
		if existing != nil {
			toVerify = append(toVerify, ia) // 覆盖要先求证，统一在循环后有界并发地做
			continue
		}
		commit(ia, false)
	}

	// 覆盖前求证：有界并发 + 总时限（见 verifyImportOverwrites）。每个号求证完立刻落盘入池，
	// 不等整批——求证已把文件里的 refresh token 轮换了，拖着不提交会让它只活在内存里。
	if len(toVerify) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), importVerifyBudget)
		p.verifyImportOverwrites(ctx, toVerify, func(k int, verdict upstream.VerifyVerdict, ran bool) {
			ia := toVerify[k]
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ran && verdict == upstream.VerifyAlive:
				commit(ia, true)
			case !ran:
				skipped++
				errs = append(errs, fmt.Sprintf("%s: 批量求证超时（开工总限 %s）或请求已断开，保留原有凭据（uid=%s），请分批重试", ia.src, importVerifyBudget, ia.uid))
			default:
				skipped++
				errs = append(errs, fmt.Sprintf("%s: 新凭据校验未通过（%s），保留原有凭据", ia.src, verdict))
			}
		})
		cancel()
	}

	// 上游动作（签到/激活/查余额）放在落盘之后统一做：它们每号几次往返，
	// 不能夹在求证与落盘之间拖慢已轮换令牌的提交。
	for _, ia := range committed {
		p.importFollowUp(ia)
	}

	total = len(accounts)
	if dryRun {
		log.Printf("panel: cockpit import preview total=%d would_import=%d skipped=%d creates=%d overwrites=%d",
			total, imported, skipped, creates, overwrites)
	} else {
		log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d creates=%d overwrites=%d",
			total, imported, skipped, creates, overwrites)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"dry_run":    dryRun,
		"total":      total,
		"imported":   imported, // dryRun 时是「将会导入」的条数
		"skipped":    skipped,
		"creates":    creates,
		"overwrites": overwrites,
		"errors":     errs,
		"warnings":   warnings, // 已入池但凭据没能落盘的号（新令牌只在内存里）
	})
}

// utf8BOM 是记事本 / Windows 工具导出的常见前缀，json.Unmarshal 不认它。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// cockpitWrapperKeys 是「账号数组被包在对象里」时可能出现的外壳字段名，按优先级尝试。
// 不放 "account"：那是 auth 文件形状里的账号对象，不是外壳。
var cockpitWrapperKeys = []string{
	"accounts", "data", "items", "list", "result", "results",
	"users", "rows", "records", "exported_accounts", "cockpit_accounts",
}

// unwrapCockpitRaw 把「外面还多包了一层」的输入剥回 decodeCockpitAccounts 认得的形态。
//
// 上游解析器（PR #196）已经覆盖数组 / 单层信封 / {account,auth} / 驼峰 / 字典，但
// 2026-10 现场遇到的这三类它仍然吃不下，而且都不是用户写错了：
//  1. UTF-8 BOM：记事本和 Windows 导出工具的默认行为；
//  2. 二次编码：把 JSON 当字符串又包了一层引号（"…"）；
//  3. 双层信封：{"version":2,"data":{"accounts":[…]}}，导出工具多套了一层。
//
// 只沿信封键下潜，且对象本身能解出 token 时立刻停手——那种是真正的账号对象，
// 继续往下剥会把「账号」当成「外壳」。信封名永远不会被当成 uid（上游 #196 的坑）。
func unwrapCockpitRaw(raw []byte) []byte {
	cur := raw
	for i := 0; i < 4; i++ {
		cur = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(cur), utf8BOM))
		if len(cur) == 0 {
			return cur
		}
		switch cur[0] {
		case '"':
			var str string
			if err := json.Unmarshal(cur, &str); err != nil {
				return cur
			}
			cur = []byte(str)
		case '{':
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(cur, &obj); err != nil {
				return cur
			}
			if _, ok := decodeCockpitOne(cur); ok {
				return cur
			}
			next, ok := cockpitEnvelopeValue(obj)
			if !ok {
				return cur
			}
			cur = next
		default:
			return cur
		}
	}
	return cur
}

// cockpitEnvelopeValue 按 cockpitWrapperKeys 的优先顺序取第一个「值是数组或对象」的外壳。
// 键名大小写不敏感（"Accounts" / "Data" 也认）。
func cockpitEnvelopeValue(obj map[string]json.RawMessage) ([]byte, bool) {
	for _, want := range cockpitWrapperKeys {
		var v json.RawMessage
		ok := false
		if vv, hit := obj[want]; hit {
			v, ok = vv, true
		} else {
			for k, vv := range obj {
				if strings.EqualFold(k, want) {
					v, ok = vv, true
					break
				}
			}
		}
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		if len(v) > 0 && (v[0] == '[' || v[0] == '{') {
			return v, true
		}
	}
	return nil, false
}

// fillImportExtras 补上游解析器不建模、但我们确实要用的两个字段：
// realm（决定分区）和 device_token（设备指纹基座）。只填空值，不覆盖解析结果。
func fillImportExtras(acc *cockpitAccount, raw json.RawMessage) {
	var flat struct {
		Realm        string `json:"realm"`
		DeviceToken  string `json:"device_token"`
		DeviceToken2 string `json:"deviceToken"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil {
		if strings.TrimSpace(acc.Realm) == "" {
			acc.Realm = flat.Realm
		}
		if strings.TrimSpace(acc.DeviceToken) == "" {
			acc.DeviceToken = firstNonEmpty(flat.DeviceToken, flat.DeviceToken2)
		}
	}
	if strings.TrimSpace(acc.Realm) != "" && strings.TrimSpace(acc.DeviceToken) != "" {
		return
	}
	// 嵌套形态把 realm / device_token 放在 auth 里
	var nested struct {
		Auth struct {
			Realm        string `json:"realm"`
			DeviceToken  string `json:"device_token"`
			DeviceToken2 string `json:"deviceToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return
	}
	if strings.TrimSpace(acc.Realm) == "" {
		acc.Realm = nested.Auth.Realm
	}
	if strings.TrimSpace(acc.DeviceToken) == "" {
		acc.DeviceToken = firstNonEmpty(nested.Auth.DeviceToken, nested.Auth.DeviceToken2)
	}
}

// addImportedAccount 把一份导入的凭证放进池子，返回它是否为新号。
//
// 与浏览器登录的分工不同：登录是一个号一个号人工确认，导入是批量灌凭证，来路/健康度
// 都未经验证，故**默认暂停选号**——先把号放进池子（签到 / 活跃 / 保活 / 余额刷新照常
// 跑起来），但不进转发路径；面板看积分、必要时手工试一次，确认没问题再「恢复选号」。
//
// 顺序有讲究：Pool.Revive 是「解冻=全清」语义，会把 paused 一并清掉，所以 Hold 必须
// 排在 Revive 之后，否则暂停会被静默撤销。Hold 还额外打「待确认」标记，让暂停能挺过
// 后续任何一次解冻/保活，直到人工「恢复选号」。
func (p *Panel) addImportedAccount(a *auth.Auth) bool {
	isNew := p.cfg.Pool.AuthByUID(a.UID) == nil
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	// 用 Hold 而非 Pause：Pause 只是普通暂停，一次「解冻」（Revive 清 paused）或
	// 人工解冻就会把号推进选号池；Hold 带「待确认」标记，解冻清不掉，只有面板上
	// 「恢复选号」（Resume）才解除。
	p.cfg.Pool.Hold(a.UID)
	return isNew
}

// importAcct 一个通过本地校验、待落盘的导入账号。
type importAcct struct {
	a        *auth.Auth
	src, uid string
	realm    string
	isNew    bool // 池里原先没有：导入后启动新号任务
}

// importVerifyConcurrency 覆盖前求证（每号一次 refresh）的并发上限：串行时几百个已有号要等几十分钟，
// 请求先被反向代理 / 浏览器掐断；并发太高又像批量刷 refresh 端点。
// importVerifyBudget 是求证「开工」的总时限：到点后不再为没轮到的号发 refresh，它们一律保留原凭据、
// 由人分批重试。已发出的 refresh 不打断（打断会丢掉已轮换的令牌），所以请求最坏要等
// importVerifyBudget + 一次 refresh 的超时。两者是变量只为让测试不必真等 60 秒。
var (
	importVerifyConcurrency = 8
	importVerifyBudget      = 60 * time.Second
)

// verifyImportOverwrites 用 refresh 求证导入的新凭据还活着，有界并发，开工受 ctx 约束。每号求证完立刻调
// done（并发调用，调用方自己加锁）；没轮到的号也调一次（ran=false），保证每个号恰好一次回调。
func (p *Panel) verifyImportOverwrites(ctx context.Context, list []importAcct, done func(k int, verdict upstream.VerifyVerdict, ran bool)) {
	sem := make(chan struct{}, importVerifyConcurrency)
	var wg sync.WaitGroup
	k := 0
loop:
	for ; k < len(list); k++ {
		// select 在两路都就绪时随机选一路，先判一次 ctx，已结束就一个也不再发。
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(k int) {
			defer func() { <-sem; wg.Done() }()
			done(k, p.cfg.Upstream.VerifySessionByRefresh(list[k].a), true)
		}(k)
	}
	for ; k < len(list); k++ {
		done(k, upstream.VerifyInconclusive, false)
	}
	wg.Wait()
}

// importFollowUp 导入后顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
func (p *Panel) importFollowUp(ia importAcct) {
	a, uid := ia.a, ia.uid
	if ia.realm == "global" {
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
		// 企业版 / 静态 API Key 无个人成长体系：CN 签到端点对企业号直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但下方余额查询
		// 照常（企业额度走 get-enterprise-user-usage 口径）。
		if !a.SupportsPersonalGrowth() {
			log.Printf("panel: import 跳过签到 uid=%s（企业版/API Key 无个人成长体系）", uid)
		} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("panel: import checkin uid=%s: %v", uid, err)
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
	}
	if ia.isNew {
		p.StartNewAccountTasks(uid)
	}
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

// ---------------------------------------------------------------------------
// 导入格式归一化
// ---------------------------------------------------------------------------

// nestedCockpit 是 cockpit tools「账号详情」导出的嵌套形态：
//
//	{"account":{"uid":"…","nickname":"…"},"auth":{"accessToken":"…","refreshToken":"…"}}
//
// 同一个导出工具会产出两种形态：批量导出是「顶层数组 + snake_case」，单账号详情
// 导出是上面这种嵌套驼峰。用户不会区分，故在解码处统一，而不是让人手工改写。
type nestedCockpit struct {
	Account struct {
		UID           string `json:"uid"`
		Nickname      string `json:"nickname"`
		EnterpriseID  string `json:"enterpriseId"`
		EnterpriseID2 string `json:"enterprise_id"`
	} `json:"account"`
	Auth struct {
		AccessToken   string `json:"accessToken"`
		AccessToken2  string `json:"access_token"`
		RefreshToken  string `json:"refreshToken"`
		RefreshToken2 string `json:"refresh_token"`
		Domain        string `json:"domain"`
		ExpiresAt     int64  `json:"expiresAt"`
		ExpiresAt2    int64  `json:"expires_at"`
	} `json:"auth"`
}

// decodeCockpitAccounts 把导入 JSON 归一成账号列表。接受：
//   - 顶层数组（批量导出）
//   - 单个扁平对象（蛇形或驼峰）
//   - 单个嵌套对象 {account,auth}（账号详情导出）
//   - 信封 {accounts|data|list|items|results|auths: [...]}
//   - 字典 {"<uid>": {...}}（键即 uid）
//
// 认不出的结构才返回 error（调用方转 400）；认得出但缺字段的条目照旧进主循环
// 记 skipped——错误信息里要能说出是哪一条，不能被解码层吞掉。
func decodeCockpitAccounts(raw []byte) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(unwrapCockpitRaw(raw))
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		return decodeCockpitItems(items), nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		for _, k := range []string{"accounts", "data", "list", "items", "results", "auths"} {
			sub, ok := obj[k]
			if !ok {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(sub, &items); err == nil {
				return decodeCockpitItems(items), nil
			}
			// 信封的值是**单个账号对象**（用户把单账号详情又包了一层）：
			// 按条目解，但绝不能拿信封键当 uid——见字典分支的说明。
			if acc, ok := decodeCockpitOne(sub); ok {
				return []cockpitAccount{acc}, nil
			}
		}
		if _, ok := obj["account"]; ok {
			return decodeCockpitItems([]json.RawMessage{trimmed}), nil
		}
		if _, ok := obj["auth"]; ok {
			return decodeCockpitItems([]json.RawMessage{trimmed}), nil
		}
		if acc, ok := decodeCockpitOne(trimmed); ok {
			return []cockpitAccount{acc}, nil
		}
		// 字典：{"<uid>": {...}}，键当 uid 兜底
		//
		// 信封名不能当 uid：`{"accounts":{"refresh_token":"r"}}` 是「信封的值是单个
		// 对象」形态，走到这里时 accounts 这个键会命中内层（有 token），兜底就会把
		// 账号的 uid 写成字面量 "accounts"——池里从此多一个格式合法、内容垃圾的号。
		// 信封名本身不是 uid（真实 uid 是手机号/UUID），直接跳过。
		keys := make([]string, 0, len(obj))
		for k := range obj {
			if isCockpitEnvelopeKey(k) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys) // 稳定顺序，便于前端展示与测试比对
		out := make([]cockpitAccount, 0, len(keys))
		for _, k := range keys {
			acc, ok := decodeCockpitOne(obj[k])
			if !ok {
				continue
			}
			if strings.TrimSpace(acc.UID) == "" {
				acc.UID = k
			}
			out = append(out, acc)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("无法识别的 JSON 结构：既不是账号数组，也不是 {account,auth} 或 {\"<uid>\":{…}}")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected JSON array or object")
	}
}

// isCockpitEnvelopeKey 报告键名是否是导出工具的信封名（不是 uid）。
func isCockpitEnvelopeKey(k string) bool {
	for _, e := range []string{"accounts", "data", "list", "items", "results", "auths"} {
		if strings.EqualFold(strings.TrimSpace(k), e) {
			return true
		}
	}
	return false
}

// decodeCockpitItems 逐条解码；认不出的条目保留一个空占位，
// 让主循环按「missing required fields」记账，而不是从结果里凭空消失。
func decodeCockpitItems(items []json.RawMessage) []cockpitAccount {
	out := make([]cockpitAccount, 0, len(items))
	for _, it := range items {
		if acc, ok := decodeCockpitOne(it); ok {
			fillImportExtras(&acc, it)
			out = append(out, acc)
			continue
		}
		out = append(out, cockpitAccount{})
	}
	return out
}

// decodeCockpitOne 依次尝试：蛇形扁平 → 驼峰扁平 → {account,auth} 嵌套。
// 判据统一是「拿到至少一个 token」：cockpitAccount 的 tag 是蛇形，直接
// Unmarshal 一个驼峰对象会成功但字段全空，不检查就会把有效账号当成空账号。
func decodeCockpitOne(raw json.RawMessage) (cockpitAccount, bool) {
	var a cockpitAccount
	if err := json.Unmarshal(raw, &a); err == nil && (a.AccessToken != "" || a.RefreshToken != "") {
		return a, true
	}
	var f struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if err := json.Unmarshal(raw, &f); err == nil && (f.AccessToken != "" || f.RefreshToken != "") {
		a.UID = f.UID
		a.Nickname = f.Nickname
		a.AccessToken = f.AccessToken
		a.RefreshToken = f.RefreshToken
		a.ExpiresAt = f.ExpiresAt
		a.Domain = f.Domain
		a.EnterpriseIDCamel = f.EnterpriseID
		return a, true
	}
	var n nestedCockpit
	if err := json.Unmarshal(raw, &n); err == nil {
		at := firstNonEmpty(n.Auth.AccessToken, n.Auth.AccessToken2)
		rt := firstNonEmpty(n.Auth.RefreshToken, n.Auth.RefreshToken2)
		if at != "" || rt != "" {
			return cockpitAccount{
				UID:               n.Account.UID,
				Nickname:          n.Account.Nickname,
				EnterpriseIDCamel: firstNonEmpty(n.Account.EnterpriseID, n.Account.EnterpriseID2),
				AccessToken:       at,
				RefreshToken:      rt,
				Domain:            n.Auth.Domain,
				ExpiresAt:         firstNonZero(n.Auth.ExpiresAt, n.Auth.ExpiresAt2),
			}, true
		}
	}
	return a, false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstNonZero(v ...int64) int64 {
	for _, n := range v {
		if n != 0 {
			return n
		}
	}
	return 0
}

// normalizeExpiresAtMillis 把 expires_at 统一成毫秒。
// 批量导出是毫秒（≈1.7e12），单账号详情导出是秒（≈1.7e9）。
// 以 1e11 分界：秒制要走 3000 年才到这个数，毫秒制的历史值也远大于它。
func normalizeExpiresAtMillis(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 1e11 {
		return v * 1000
	}
	return v
}
