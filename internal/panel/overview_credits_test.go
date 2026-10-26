package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// overview 账号行的 credits_used_today 是四态契约，不是"有数就显示数字"：
//
//	记录器未装配          → 字段缺席（「—」）
//	今天没有该账号的桶    → 0（本网关今天确实没为它花额度）
//	有桶且至少一份回执    → CR 合计（免费/限免模型合法为 0）
//	有桶但零回执          → 字段缺席（花费未知，不能谎报 0）
//
// 最后两行是本次修的重点：Add 对每次尝试都建桶，所以"有桶"只证明有请求。把
// 零回执写成 0 就是把"没测到"当"没花钱"，与 usage 包注释和 CRN 的语义冲突。
func TestOverviewTodayCreditsMatrix(t *testing.T) {
	newPool := func() *pool.Pool {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"}) // 有多笔回执
		p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at"}) // 今天零流量
		p.Add(&auth.Auth{UID: "u3", Nickname: "C", AccessToken: "at"}) // 有请求、零回执
		p.Add(&auth.Auth{UID: "u4", Nickname: "D", AccessToken: "at"}) // 回执金额为 0
		return p
	}
	now := time.Now()
	rec := usage.New("")
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{Credit: 1.5, HasCredit: true}, true)
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{Credit: 0.5, HasCredit: true}, true)
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true) // 混一笔无回执：不影响
	rec.Add(now, "cn", "u3", "glm-5.2", usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	// 限免模型：回执确实来了，只是金额 0 → 必须显示 0，不能"—"。
	rec.Add(now, "cn", "u4", "glm-5.2", usage.Delta{Credit: 0, HasCredit: true}, true)

	// overview 原样返回 accounts[]；uid → 该行的 credits_used_today 原始 JSON
	// （用 RawMessage 才能区分"字段缺席"与"值为 0"）。
	get := func(pn *Panel) map[string]json.RawMessage {
		t.Helper()
		rr := httptest.NewRecorder()
		pn.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("overview -> %d %s", rr.Code, rr.Body)
		}
		var raw struct {
			Accounts []map[string]json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		m := make(map[string]json.RawMessage, len(raw.Accounts))
		for _, a := range raw.Accounts {
			var uid string
			if err := json.Unmarshal(a["uid"], &uid); err != nil {
				t.Fatal(err)
			}
			m[uid] = a["credits_used_today"]
		}
		return m
	}
	wantAbsent := func(t *testing.T, m map[string]json.RawMessage, uid string) {
		t.Helper()
		if raw, ok := m[uid]; ok && len(raw) != 0 {
			t.Fatalf("%s 应缺席 credits_used_today（「—」），得到 %s；全量 %v", uid, raw, m)
		}
	}
	wantCredits := func(t *testing.T, m map[string]json.RawMessage, uid string, want float64) {
		t.Helper()
		raw, ok := m[uid]
		if !ok || len(raw) == 0 {
			t.Fatalf("%s 缺少 credits_used_today，want %v；全量 %v", uid, want, m)
		}
		var got float64
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s credits_used_today = %s, want %v", uid, raw, want)
		}
		if got != want {
			t.Fatalf("%s credits_used_today = %v, want %v", uid, got, want)
		}
	}

	pn := New(Config{Version: "test", Pool: newPool(), Usage: rec})
	got := get(pn)
	wantCredits(t, got, "u1", 2) // 1.5+0.5，无回执那笔不参与
	wantCredits(t, got, "u2", 0) // 今天零流量 → 0（不是「—」）
	wantAbsent(t, got, "u3")     // 有请求、零回执 → 花费未知
	wantCredits(t, got, "u4", 0) // 回执金额 0 → 0（与「—」区分开）

	// 开代理池的分支同样要注入——否则一开代理今日积分就消失。
	pp := proxypool.New("", nil)
	t.Cleanup(pp.Close)
	pnProxy := New(Config{Version: "test", Pool: newPool(), Proxypool: pp, Usage: rec})
	gotProxy := get(pnProxy)
	wantCredits(t, gotProxy, "u1", 2)
	wantCredits(t, gotProxy, "u2", 0)
	wantAbsent(t, gotProxy, "u3")

	// 没记录器：所有行都缺席（前端渲染「—」），连零流量账号也不能给 0。
	noRec := get(New(Config{Version: "test", Pool: newPool()}))
	if len(noRec) != 4 {
		t.Fatalf("账号行数 = %d, want 4", len(noRec))
	}
	for uid := range noRec {
		wantAbsent(t, noRec, uid)
	}
}

// 积分注入重构不能改变代理注入语义：无代理池的分支仍产出「ProxyTag 空 + ProxyOK
// false」的行；有代理池时未绑定的账号也绝不去 views 表里按空 ProxyID 取节点。
func TestOverviewCreditsKeepsProxySemantics(t *testing.T) {
	newPool := func() *pool.Pool {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
		p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at"})
		return p
	}
	type row struct {
		UID      string `json:"uid"`
		ProxyID  string `json:"proxy_id"`
		ProxyTag string `json:"proxy_tag"`
		ProxyOK  bool   `json:"proxy_ok"`
	}
	rowsOf := func(pn *Panel) map[string]row {
		t.Helper()
		rr := httptest.NewRecorder()
		pn.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
		var d struct {
			Accounts []row `json:"accounts"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		m := make(map[string]row, len(d.Accounts))
		for _, a := range d.Accounts {
			m[a.UID] = a
		}
		return m
	}

	// Proxypool == nil：每行 ProxyTag 为空、ProxyOK=false（与重构前逐位一致）。
	for uid, r := range rowsOf(New(Config{Version: "test", Pool: newPool()})) {
		if r.ProxyID != "" || r.ProxyTag != "" || r.ProxyOK {
			t.Fatalf("无代理池时 %s 应无任何代理字段值，得到 %+v", uid, r)
		}
	}

	// Proxypool != nil 但账号未绑定：不能因为查了空 ProxyID 而拿到别人的节点。
	pp := proxypool.New("", nil)
	t.Cleanup(pp.Close)
	res, err := pp.AddRaw("http://u:p@127.0.0.1:1#node-one")
	if err != nil || res.Imported != 1 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	p := newPool()
	p.SetAccountProxy("u1", pp.List()[0].ID) // u1 绑定；u2 保持空
	m := rowsOf(New(Config{Version: "test", Pool: p, Proxypool: pp}))
	if r := m["u1"]; r.ProxyTag != "node-one" || !r.ProxyOK {
		t.Fatalf("u1 绑定节点未注入: %+v", r)
	}
	if r := m["u2"]; r.ProxyID != "" || r.ProxyTag != "" || r.ProxyOK {
		t.Fatalf("u2 未绑定却拿到代理字段: %+v", r)
	}
}

// overviewCreditsOf 打真实的 /panel/api/overview，返回 uid → credits_used_today 原始
// JSON（nil 表示字段缺席）。用 RawMessage 才能把「缺席」与「0」分开——两者都是
// 合法的对外状态，混淆正是本组测试要防的事。
func overviewCreditsOf(t *testing.T, pn *Panel) map[string]json.RawMessage {
	t.Helper()
	rr := httptest.NewRecorder()
	pn.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("overview -> %d %s", rr.Code, rr.Body)
	}
	var raw struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Accounts) == 0 {
		t.Fatal("overview 没有 accounts 行")
	}
	m := make(map[string]json.RawMessage, len(raw.Accounts))
	for _, a := range raw.Accounts {
		var uid string
		if err := json.Unmarshal(a["uid"], &uid); err != nil {
			t.Fatal(err)
		}
		m[uid] = a["credits_used_today"]
	}
	return m
}

// 记录器装着、但**整池今天一条桶都没有**：每行必须是 0，不是「—」。
//
// 这条专打一个真实缺陷（线上冒烟 states D/E 抓到、原矩阵测试漏掉）：映射曾用
// `if credits != nil` 判「有没有记录器」，而 TodayCreditsByUID 在当天无桶时返回
// nil。于是整池空闲（夜间 / 刚部署 / 单号池）时那唯一的 nil 被读成「没装配
// 记录器」，所有行一起退化成「—」，0 分支根本走不到。原矩阵测试里 u2 是无流量
// 账号，但 u1/u3/u4 有今天的桶，把 credits 顶成了非 nil，0 分支是**碰巧**被执行
// 到的——所以必须补一个「全局无今日桶」的用例。
func TestOverviewTodayCreditsIdlePool(t *testing.T) {
	newPool := func() *pool.Pool {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
		p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at"})
		return p
	}
	// 只有昨天的桶：今天整池无流量。
	yday := time.Now().AddDate(0, 0, -1)
	rec := usage.New("")
	rec.Add(yday, "cn", "u1", "glm-5.2", usage.Delta{Credit: 9, HasCredit: true}, true)
	rec.Add(yday, "cn", "u2", "glm-5.2", usage.Delta{Credit: 7, HasCredit: true}, true)
	if credits := rec.TodayCreditsByUID(time.Now()); credits != nil {
		t.Fatalf("前置条件不成立：今天不该有桶，得到 %v", credits)
	}

	got := overviewCreditsOf(t, New(Config{Version: "test", Pool: newPool(), Usage: rec}))
	if len(got) != 2 {
		t.Fatalf("账号行数 = %d, want 2", len(got))
	}
	for uid, raw := range got {
		if len(raw) == 0 {
			t.Fatalf("%s 整池空闲时必须给 0（记录器在、今天没为它花钱），实际字段缺席：%v", uid, got)
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v != 0 {
			t.Fatalf("%s credits_used_today = %s, want 0", uid, raw)
		}
	}
}

// 记录器装着但一个桶都没有（全新部署）：同样每行 0。
// 与上一条的区别只在于「连历史都没有」，结论必须一致——判据是记录器是否存在，
// 不是桶表里有什么。
func TestOverviewTodayCreditsEmptyRecorder(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at"})
	rec := usage.New("") // 零桶

	got := overviewCreditsOf(t, New(Config{Version: "test", Pool: p, Usage: rec}))
	if len(got) != 2 {
		t.Fatalf("账号行数 = %d, want 2", len(got))
	}
	for uid, raw := range got {
		if len(raw) == 0 {
			t.Fatalf("%s 空记录器也必须给 0（不是「—」），实际字段缺席：%v", uid, got)
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v != 0 {
			t.Fatalf("%s credits_used_today = %s, want 0", uid, raw)
		}
	}
}
