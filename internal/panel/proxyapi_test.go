package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// newProxyPanel 构造带代理池的面板：两个账号（u1/u2）+ 一个 http 类型节点
// （指向本地假 CONNECT 代理 fixture 的地址占位——绑定/列表/订阅接口不需要真实可拨）。
func newProxyPanel(t *testing.T) (*Panel, *pool.Pool, *proxypool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})
	pp := proxypool.New("", nil)
	t.Cleanup(pp.Close)
	// 与 main 装配一致：节点删除回调 → 池批量解绑。
	pp.SetUnbindCallback(func(ids []string) []string {
		idSet := make(map[string]bool, len(ids))
		for _, id := range ids {
			idSet[id] = true
		}
		return p.ClearProxyIfMatch(idSet)
	})
	// http 类型节点：解析/绑定语义不需要真实握手；可拨性测试在 proxypool 包覆盖。
	res, err := pp.AddRaw("http://u:p@127.0.0.1:1#node-one\nss://YWVzLTI1Ni1nY206cA@127.0.0.1:2#node-two")
	if err != nil || res.Imported != 2 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	pn := New(Config{Version: "test", Pool: p, Proxypool: pp})
	return pn, p, pp
}

func doJSONReq(t *testing.T, pn *Panel, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestBindProxyAPI 绑定/解绑/404（uid 不存在 / 节点不存在）。
func TestBindProxyAPI(t *testing.T) {
	pn, p, pp := newProxyPanel(t)
	nodeID := pp.List()[0].ID

	// uid 不存在 → 404 "account not found"。
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/accounts/ghost/bind_proxy", map[string]any{"proxy_id": nodeID})
	if rec.Code != http.StatusNotFound || body["error"] != "account not found" {
		t.Fatalf("ghost: code=%d body=%v", rec.Code, body)
	}
	// 节点不存在 → 404 "proxy not found"。
	rec, body = doJSONReq(t, pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy", map[string]any{"proxy_id": "nope"})
	if rec.Code != http.StatusNotFound || body["error"] != "proxy not found" {
		t.Fatalf("bad proxy: code=%d body=%v", rec.Code, body)
	}
	// 绑定 → ok + proxy_tag。
	rec, body = doJSONReq(t, pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy", map[string]any{"proxy_id": nodeID})
	if rec.Code != http.StatusOK || body["ok"] != true || body["proxy_tag"] != "node-one" {
		t.Fatalf("bind: code=%d body=%v", rec.Code, body)
	}
	if got := p.AccountProxy("u1"); got != nodeID {
		t.Fatalf("pool 未落绑定: %q", got)
	}
	// 解绑（proxy_id=""）→ ok + 空 tag。
	rec, body = doJSONReq(t, pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy", map[string]any{"proxy_id": ""})
	if rec.Code != http.StatusOK || body["ok"] != true || body["proxy_tag"] != "" {
		t.Fatalf("unbind: code=%d body=%v", rec.Code, body)
	}
	if got := p.AccountProxy("u1"); got != "" {
		t.Fatalf("解绑未生效: %q", got)
	}
}

// TestProxiesListAccountsCount 列表 accounts 计数：两个账号绑同一节点 → accounts=2。
func TestProxiesListAccountsCount(t *testing.T) {
	pn, _, pp := newProxyPanel(t)
	nodeID := pp.List()[0].ID
	if !pn.cfg.Pool.SetAccountProxy("u1", nodeID) {
		t.Fatal("bind u1")
	}
	if !pn.cfg.Pool.SetAccountProxy("u2", nodeID) {
		t.Fatal("bind u2")
	}
	rec, body := doJSONReq(t, pn, http.MethodGet, "/panel/api/proxies", nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("list: code=%d body=%v", rec.Code, body)
	}
	proxies, _ := body["proxies"].([]any)
	if len(proxies) != 2 {
		t.Fatalf("proxies=%v", body["proxies"])
	}
	first, _ := proxies[0].(map[string]any)
	wantFields := []string{"id", "tag", "type", "server", "port", "sub_id", "enabled", "latency_ms", "checked_at", "fails", "accounts"}
	for _, k := range wantFields {
		if _, ok := first[k]; !ok {
			t.Fatalf("契约字段缺失: %s（got %v）", k, first)
		}
	}
	var accountsSum int
	for _, it := range proxies {
		m, _ := it.(map[string]any)
		if m["accounts"].(float64) > 0 {
			accountsSum += int(m["accounts"].(float64))
		}
	}
	if accountsSum != 2 {
		t.Fatalf("accounts 计数=%d want 2", accountsSum)
	}
	// subs 形状。
	subs, _ := body["subs"].([]any)
	if len(subs) != 0 {
		t.Fatalf("subs=%v", subs)
	}
}

// TestProxiesMutations 导入 / toggle / delete（含解绑）。
func TestProxiesMutations(t *testing.T) {
	pn, p, pp := newProxyPanel(t)
	// 导入。
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/proxies", map[string]any{"text": "ss://YWVzLTI1Ni1nY206cA@3.3.3.3:8388#imported"})
	if rec.Code != http.StatusOK || body["ok"] != true || body["imported"].(float64) != 1 {
		t.Fatalf("import: code=%d body=%v", rec.Code, body)
	}
	imported := ""
	for _, n := range pp.List() {
		if n.Tag == "imported" {
			imported = n.ID
		}
	}
	if imported == "" {
		t.Fatal("imported node missing")
	}
	// toggle。
	rec, body = doJSONReq(t, pn, http.MethodPost, "/panel/api/proxies/"+imported+"/toggle", nil)
	if rec.Code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("toggle: code=%d body=%v", rec.Code, body)
	}
	// 绑定后删除 → 自动解绑。
	if !p.SetAccountProxy("u1", imported) {
		t.Fatal("bind")
	}
	rec, body = doJSONReq(t, pn, http.MethodDelete, "/panel/api/proxies/"+imported, nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete: code=%d body=%v", rec.Code, body)
	}
	if got := p.AccountProxy("u1"); got != "" {
		t.Fatalf("删除后未解绑: %q", got)
	}
	// 未知节点 404。
	rec, _ = doJSONReq(t, pn, http.MethodDelete, "/panel/api/proxies/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: code=%d", rec.Code)
	}
}

// TestSubscriptionsLifecycle 用 httptest 充当订阅源：create+sync → 再 sync →
// delete（removed_nodes / unbound_accounts）。
func TestSubscriptionsLifecycle(t *testing.T) {
	const subYAML = `proxies:
  - name: sub-node-a
    type: ss
    server: 1.1.1.1
    port: 8388
    cipher: aes-256-gcm
    password: p
  - name: sub-node-b
    type: ss
    server: 2.2.2.2
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subYAML)
	}))
	defer srv.Close()

	pn, p, _ := newProxyPanel(t)
	// 创建 + 立即同步。
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions", map[string]any{"name": "机场", "url": srv.URL})
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("create: code=%d body=%v", rec.Code, body)
	}
	if body["total"].(float64) != 2 || body["imported"].(float64) != 2 {
		t.Fatalf("create sync: %v", body)
	}
	sub, _ := body["sub"].(map[string]any)
	if sub["count"].(float64) != 2 || sub["name"] != "机场" || sub["url"] != srv.URL {
		t.Fatalf("sub=%v", sub)
	}
	subID := sub["id"].(string)

	// 绑定一个账号到 sub 节点，然后删除订阅 → removed_nodes=2, unbound_accounts=1。
	var subNodeID string
	for _, n := range pn.cfg.Proxypool.List() {
		if n.Tag == "sub-node-a" {
			subNodeID = n.ID
		}
	}
	p.SetAccountProxy("u1", subNodeID)
	rec, body = doJSONReq(t, pn, http.MethodDelete, "/panel/api/subscriptions/"+subID, nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete sub: code=%d body=%v", rec.Code, body)
	}
	if body["removed_nodes"].(float64) != 2 || body["unbound_accounts"].(float64) != 1 {
		t.Fatalf("delete sub: %v", body)
	}
	if got := p.AccountProxy("u1"); got != "" {
		t.Fatalf("订阅删除后未解绑: %q", got)
	}
	// 重复删除 404。
	rec, _ = doJSONReq(t, pn, http.MethodDelete, "/panel/api/subscriptions/"+subID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double delete: code=%d", rec.Code)
	}
}

// TestSyncSubscriptionEndpoint 手动同步端点（机场内容已变化 → 整体替换）。
func TestSyncSubscriptionEndpoint(t *testing.T) {
	subYAML := `proxies:
  - name: s1
    type: ss
    server: 1.1.1.1
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subYAML)
	}))
	defer srv.Close()

	pn, _, _ := newProxyPanel(t)
	_, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions", map[string]any{"url": srv.URL})
	sub, _ := body["sub"].(map[string]any)
	subID := sub["id"].(string)

	// 内容变化：s1 消失、s2 出现。
	subYAML = `proxies:
  - name: s2
    type: ss
    server: 2.2.2.2
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions/"+subID+"/sync", nil)
	if rec.Code != http.StatusOK || body["imported"].(float64) != 1 {
		t.Fatalf("sync: code=%d body=%v", rec.Code, body)
	}
	tags := map[string]bool{}
	for _, n := range pn.cfg.Proxypool.List() {
		tags[n.Tag] = true
	}
	if tags["s1"] || !tags["s2"] {
		t.Fatalf("整体替换未生效: %v", tags)
	}
	// 未知订阅 → 404（not-found 与「拉取/解析失败」必须是不同出口：后者 502）。
	rec, body = doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions/none/sync", nil)
	if rec.Code != http.StatusNotFound || body["ok"] != false {
		t.Fatalf("unknown sub 应 404: code=%d body=%v", rec.Code, body)
	}
}

// TestOverviewProxyFields overview 账号项注入 proxy_id/proxy_tag/proxy_ok。
func TestOverviewProxyFields(t *testing.T) {
	pn, p, pp := newProxyPanel(t)
	nodeID := pp.List()[0].ID // node-one（enabled）
	p.SetAccountProxy("u1", nodeID)
	// u2 绑一个已停用节点 → proxy_ok=false。
	disabled := pp.List()[1].ID
	if _, err := pp.Toggle(disabled); err != nil {
		t.Fatal(err)
	}
	p.SetAccountProxy("u2", disabled)

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var d struct {
		Accounts []struct {
			UID      string `json:"uid"`
			ProxyID  string `json:"proxy_id"`
			ProxyTag string `json:"proxy_tag"`
			ProxyOK  bool   `json:"proxy_ok"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	byUID := map[string]struct {
		UID      string `json:"-"`
		ProxyID  string
		ProxyTag string
		ProxyOK  bool
	}{}
	for _, a := range d.Accounts {
		byUID[a.UID] = struct {
			UID      string `json:"-"`
			ProxyID  string
			ProxyTag string
			ProxyOK  bool
		}{a.UID, a.ProxyID, a.ProxyTag, a.ProxyOK}
	}
	if v, ok := byUID["u1"]; !ok || v.ProxyID != nodeID || v.ProxyTag != "node-one" || !v.ProxyOK {
		t.Fatalf("u1=%+v", v)
	}
	if v, ok := byUID["u2"]; !ok || v.ProxyID != disabled || v.ProxyTag == "" || v.ProxyOK {
		t.Fatalf("u2=%+v（停用节点应 proxy_ok=false）", v)
	}
}

// ---------------------------------------------------------------------------
// S9 回归：列表不泄漏节点出站凭据（Raw），但契约字段齐全
// ---------------------------------------------------------------------------

// TestProxiesListOmitsRawCredentials 列表每 5s 轮询，raw（password/uuid/obfs…）
// 不得出现在响应里。
func TestProxiesListOmitsRawCredentials(t *testing.T) {
	pn, p, pp := newProxyPanel(t)
	// 造一个带凭据的节点（ss 的 password 只在 raw 里）。
	res, err := pp.AddRaw("ss://YWVzLTI1Ni1nY206c2VjcmV0LXBhc3M@9.9.9.9:8388#secret-node")
	if err != nil || res.Imported != 1 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	var secretID string
	for _, n := range pp.List() {
		if n.Tag == "secret-node" {
			secretID = n.ID
			if n.Raw == nil || n.Raw["password"] == nil {
				t.Fatalf("fixture 应带 password: %+v", n.Raw)
			}
		}
	}
	if secretID == "" {
		t.Fatal("fixture 节点缺失")
	}
	p.SetAccountProxy("u1", secretID)

	rec, body := doJSONReq(t, pn, http.MethodGet, "/panel/api/proxies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	rawBody := rec.Body.String()
	if strings.Contains(rawBody, "raw") || strings.Contains(rawBody, "secret-pass") {
		t.Fatalf("列表泄漏了节点出站配置: %s", rawBody)
	}
	proxies, _ := body["proxies"].([]any)
	if len(proxies) != 3 {
		t.Fatalf("proxies=%d", len(proxies))
	}
	// 契约字段仍在（前端只用这些）。
	wantFields := []string{"id", "tag", "type", "server", "port", "sub_id", "enabled",
		"latency_ms", "checked_at", "fails", "accounts"}
	for _, it := range proxies {
		m, _ := it.(map[string]any)
		for _, k := range wantFields {
			if _, ok := m[k]; !ok {
				t.Fatalf("契约字段缺失 %q: %v", k, m)
			}
		}
		if _, ok := m["raw"]; ok {
			t.Fatalf("不应有 raw 字段: %v", m)
		}
	}
}

// ---------------------------------------------------------------------------
// S7 回归：导入请求体上限 + 单次节点数上限
// ---------------------------------------------------------------------------

// TestProxiesImportBodyLimit 超限请求体 → 413（修复前无上限，直接读进内存）。
func TestProxiesImportBodyLimit(t *testing.T) {
	pn, _, pp := newProxyPanel(t)
	before := len(pp.List())
	// 6 MiB 的合法 JSON（text 里塞大块 URI 备注）。
	huge := `{"text":"` + "ss://YWVzLTI1Ni1nY206cA@1.1.1.1:8388#" + strings.Repeat("x", 6<<20) + `"}`
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/proxies", strings.NewReader(huge)))
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("超限请求体应 413/400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := len(pp.List()); got != before {
		t.Fatalf("超限请求不得改动节点: %d → %d", before, got)
	}
}

// TestProxiesImportNodeLimit 单次导入节点数上限在结果里体现 skipped/errors。
func TestProxiesImportNodeLimit(t *testing.T) {
	pn, _, pp := newProxyPanel(t)
	base := len(pp.List())
	var sb strings.Builder
	for i := 0; i < 1010; i++ {
		fmt.Fprintf(&sb, "ss://YWVzLTI1Ni1nY206cA@10.1.%d.%d:8388#n%d\n", i/250, i%250, i)
	}
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/proxies", map[string]any{"text": sb.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
	if body["imported"].(float64) != 1000 {
		t.Fatalf("imported=%v want 1000", body["imported"])
	}
	if body["skipped"].(float64) != 10 {
		t.Fatalf("skipped=%v want 10", body["skipped"])
	}
	errs, _ := body["errors"].([]any)
	if len(errs) == 0 || !strings.Contains(fmt.Sprint(errs), "limit") {
		t.Fatalf("errors 应说明节点数上限: %v", body["errors"])
	}
	if got := len(pp.List()); got != base+1000 {
		t.Fatalf("池内节点数=%d want %d", got, base+1000)
	}
}

// ---------------------------------------------------------------------------
// S8 回归：sync 的 not-found 是 404，拉取失败才是 502
// ---------------------------------------------------------------------------

func TestSubscriptionSyncNotFoundIs404(t *testing.T) {
	pn, _, _ := newProxyPanel(t)
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions/nope/sync", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知订阅应 404，得到 %d: %v", rec.Code, body)
	}
	if body["ok"] != false {
		t.Fatalf("body=%v", body)
	}
}

func TestSubscriptionSyncFetchFailureIs502(t *testing.T) {
	// 指向一个已关闭的本地 listener：拉取必然失败（非 not-found）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	pn, _, pp := newProxyPanel(t)
	sub, err := pp.AddSubscription("dead", url)
	if err != nil {
		t.Fatal(err)
	}
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/subscriptions/"+sub.ID+"/sync", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("拉取失败应 502，得到 %d: %v", rec.Code, body)
	}
}

// ---------------------------------------------------------------------------
// S10 回归：bind_proxy 对停用节点的语义（允许预绑定，启用后生效）
// ---------------------------------------------------------------------------

func TestBindProxyAllowsDisabledNode(t *testing.T) {
	pn, p, pp := newProxyPanel(t)
	disabled := pp.List()[1].ID
	if _, err := pp.Toggle(disabled); err != nil {
		t.Fatal(err)
	}
	if nv, ok := pp.NodeView(disabled); !ok || nv.Enabled {
		t.Fatalf("fixture 应已停用: %+v ok=%v", nv, ok)
	}
	rec, body := doJSONReq(t, pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy", map[string]any{"proxy_id": disabled})
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("停用节点应允许预绑定: code=%d body=%v", rec.Code, body)
	}
	if got := p.AccountProxy("u1"); got != disabled {
		t.Fatalf("绑定未落池: %q", got)
	}
	// 实际出口仍是直连（停用 = 回落），但绑定意图保留；重新启用即生效。
	if got := pp.ExitKey(disabled); got != "" {
		t.Fatalf("停用节点的实际出口应为直连，得到 %q", got)
	}
	if _, err := pp.Toggle(disabled); err != nil {
		t.Fatal(err)
	}
	if got := pp.ExitKey(disabled); got != disabled {
		t.Fatalf("重新启用后实际出口应为该节点，得到 %q", got)
	}
}
