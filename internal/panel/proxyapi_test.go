package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/nodebridge"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// proxyFixture 是「桥 + 上游池 + 面板」的测试装配。
// 节点经 nodebridge 在本机开 socks5 监听，再以 socks5:// URL 进上游 Manager。
type proxyFixture struct {
	pn *Panel
	p  *pool.Pool
	pp *proxypool.Manager
	br *nodebridge.Bridge
}

// newProxyFixture 默认两个 socks5 节点（端口 1/2 不会真的被拨，仅占位）。
func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	return newProxyFixtureWith(t,
		map[string]any{"name": "node-one", "type": "socks5", "server": "127.0.0.1", "port": 1, "password": "one-secret"},
		map[string]any{"name": "node-two", "type": "socks5", "server": "127.0.0.1", "port": 2},
	)
}

// newProxyFixtureWith 按旧 proxies.json 的落盘形状造节点：外层是面板元信息，
// raw 才是 mihomo 的代理定义（桥只认 raw，没有 raw 的条目会被跳过）。
func newProxyFixtureWith(t *testing.T, raws ...map[string]any) *proxyFixture {
	t.Helper()
	nodes := map[string]any{}
	for i, raw := range raws {
		id := fmt.Sprintf("n%d", i+1)
		tag, _ := raw["name"].(string)
		nodes[id] = map[string]any{
			"id": id, "tag": tag, "type": raw["type"], "server": raw["server"],
			"port": raw["port"], "enabled": true, "raw": raw,
		}
	}
	disk := map[string]any{"version": 1, "subs": map[string]any{}, "nodes": nodes}
	blob, _ := json.Marshal(disk)
	path := filepath.Join(t.TempDir(), "proxies.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	br, err := nodebridge.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(br.Close)

	pp, err := proxypool.New("", &http.Transport{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pp.Close)
	if text := br.Lines(); text != "" {
		if _, err := pp.Import(text); err != nil {
			t.Fatal(err)
		}
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})
	pn := New(Config{Version: "test", Pool: p, Proxypool: pp, NodeBridge: br})
	return &proxyFixture{pn: pn, p: p, pp: pp, br: br}
}

// nodeID 按节点名（=桥里的 tag）取上游池 ID。
func (f *proxyFixture) nodeID(t *testing.T, tag string) string {
	t.Helper()
	for _, v := range f.pp.Snapshot().Proxies {
		if m, ok := f.br.Meta(v.URL); ok && m.Tag == tag {
			return v.ID
		}
	}
	t.Fatalf("节点 %q 不在池里", tag)
	return ""
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

// TestBindProxyAPI 绑定 → fixed、空 proxy_id → random（删条目）、mode=direct → direct，
// 以及 404（uid 不存在 / 节点不存在）。
func TestBindProxyAPI(t *testing.T) {
	f := newProxyFixture(t)
	nodeID := f.nodeID(t, "node-one")

	rec, _ := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/ghost/bind_proxy",
		map[string]any{"proxy_id": nodeID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知账号应 404，得到 %d", rec.Code)
	}

	rec, _ = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy",
		map[string]any{"proxy_id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，得到 %d", rec.Code)
	}

	rec, body := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy",
		map[string]any{"proxy_id": nodeID})
	if rec.Code != http.StatusOK || body["mode"] != "fixed" {
		t.Fatalf("绑定应成功并回 mode=fixed，得到 %d %+v", rec.Code, body)
	}
	if got := body["proxy_tag"]; got != "node-one" {
		t.Fatalf("回显的 proxy_tag 应来自桥：%+v", body)
	}
	if b := f.pp.Snapshot().Bindings["u1"]; b.Mode != "fixed" || b.ProxyID != nodeID {
		t.Fatalf("绑定未落到上游池：%+v", b)
	}

	// 空 proxy_id = 解绑 = 默认随机（随机池仍受出口约束，不会用宿主 IP）。
	rec, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy",
		map[string]any{"proxy_id": ""})
	if rec.Code != http.StatusOK || body["mode"] != "random" {
		t.Fatalf("解绑应回 mode=random，得到 %d %+v", rec.Code, body)
	}
	if _, ok := f.pp.Snapshot().Bindings["u1"]; ok {
		t.Fatal("解绑后不该留 bindings 条目")
	}

	rec, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/u2/bind_proxy",
		map[string]any{"mode": "direct"})
	if rec.Code != http.StatusOK || body["mode"] != "direct" {
		t.Fatalf("显式直连应成功，得到 %d %+v", rec.Code, body)
	}
	if b := f.pp.Snapshot().Bindings["u2"]; b.Mode != "direct" {
		t.Fatalf("direct 未落到上游池：%+v", b)
	}
}

// TestBindProxyToDisabledNode 停用节点允许预绑定（不给 4xx），重启/启用后自动生效。
func TestBindProxyToDisabledNode(t *testing.T) {
	f := newProxyFixture(t)
	nodeID := f.nodeID(t, "node-two")
	off := false
	if err := f.pp.Update(nodeID, proxypool.Input{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	rec, body := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/accounts/u1/bind_proxy",
		map[string]any{"proxy_id": nodeID})
	if rec.Code != http.StatusOK || body["mode"] != "fixed" {
		t.Fatalf("停用节点应允许预绑定，得到 %d %+v", rec.Code, body)
	}
}

// TestProxiesListAPI 列表形状：proxies 带桥元信息（tag/type/server/port），subs 是数组。
func TestProxiesListAPI(t *testing.T) {
	f := newProxyFixture(t)
	rec, body := doJSONReq(t, f.pn, http.MethodGet, "/panel/api/proxies", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，得到 %d", rec.Code)
	}
	list, ok := body["proxies"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("应有 2 个节点，得到 %+v", body["proxies"])
	}
	first := list[0].(map[string]any)
	for _, k := range []string{"id", "tag", "type", "server", "port", "enabled", "latency_ms", "accounts"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("节点行缺字段 %q：%+v", k, first)
		}
	}
	if first["type"] != "socks5" || first["server"] != "127.0.0.1" {
		t.Fatalf("节点行应回桥里的真实协议/服务器：%+v", first)
	}
	if _, ok := body["subs"].([]any); !ok {
		t.Fatalf("subs 必须是数组（空也要给 []），得到 %#v", body["subs"])
	}
}

// TestProxyListHidesSecrets 节点密码不得出现在列表响应里。
func TestProxyListHidesSecrets(t *testing.T) {
	f := newProxyFixture(t)
	rec, _ := doJSONReq(t, f.pn, http.MethodGet, "/panel/api/proxies", nil)
	if strings.Contains(rec.Body.String(), "one-secret") {
		t.Fatalf("列表泄露了节点密码：%s", rec.Body.String())
	}
}

// TestProxiesImportToggleDelete 手动导入（一行一个 URL）→ 启停 → 删除 → 404。
func TestProxiesImportToggleDelete(t *testing.T) {
	f := newProxyFixture(t)
	base := len(f.pp.Snapshot().Proxies)

	rec, body := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/proxies",
		map[string]any{"text": "socks5://u:p@3.3.3.3:1080"})
	if rec.Code != http.StatusOK || body["imported"].(float64) != 1 {
		t.Fatalf("导入应成功：%d %+v", rec.Code, body)
	}
	if got := len(f.pp.Snapshot().Proxies); got != base+1 {
		t.Fatalf("节点数应为 %d，得到 %d", base+1, got)
	}

	// 重复导入 → duplicate 计入 skipped，不报错。
	_, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/proxies",
		map[string]any{"text": "socks5://u:p@3.3.3.3:1080"})
	if body["imported"].(float64) != 0 || body["skipped"].(float64) != 1 {
		t.Fatalf("重复导入应 imported=0 skipped=1：%+v", body)
	}

	// 非法行 → 200 + errors（不整批失败）。
	rec, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/proxies",
		map[string]any{"text": "definitely-not-a-proxy-url"})
	if rec.Code != http.StatusOK || body["imported"].(float64) != 0 {
		t.Fatalf("非法行应 200 且 imported=0：%d %+v", rec.Code, body)
	}
	if errs, _ := body["errors"].([]any); len(errs) != 1 {
		t.Fatalf("非法行应回一条 errors：%+v", body)
	}

	var imported string
	for _, v := range f.pp.Snapshot().Proxies {
		if v.Name == "3.3.3.3:1080" {
			imported = v.ID
		}
	}
	if imported == "" {
		t.Fatal("找不到刚导入的节点（Name 应为 host:port）")
	}

	rec, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/proxies/"+imported+"/toggle", nil)
	if rec.Code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("启停应成功并回 enabled=false：%d %+v", rec.Code, body)
	}

	rec, _ = doJSONReq(t, f.pn, http.MethodDelete, "/panel/api/proxies/"+imported, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，得到 %d", rec.Code)
	}
	rec, _ = doJSONReq(t, f.pn, http.MethodDelete, "/panel/api/proxies/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除不存在节点应 404，得到 %d", rec.Code)
	}
}

// TestProxyImportBodyTooLarge 请求体超上限 → 413（不是 OOM，也不是 400）。
func TestProxyImportBodyTooLarge(t *testing.T) {
	f := newProxyFixture(t)
	// 必须是「合法 JSON 形状 + 超长正文」：裸垃圾会在读到上限之前就被 json 判成
	// 语法错误（400），测不到 MaxBytesReader 那条路。
	var buf bytes.Buffer
	buf.WriteString(`{"text":"`)
	buf.WriteString(strings.Repeat("a", proxyImportMaxBytes))
	buf.WriteString(`"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/proxies", bytes.NewReader(buf.Bytes()))
	f.pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，得到 %d", rec.Code)
	}
}

// TestProxySubscriptionLifecycle 建订阅（拉取→桥→池）→ 列表统计 → 同步幂等 → 删除。
func TestProxySubscriptionLifecycle(t *testing.T) {
	f := newProxyFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `
proxies:
  - {name: "sub-one", type: socks5, server: 127.0.0.1, port: 1080}
  - {name: "sub-two", type: socks5, server: 127.0.0.1, port: 1081}
`)
	}))
	defer srv.Close()

	rec, body := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/subscriptions",
		map[string]any{"name": "机场", "url": srv.URL})
	if rec.Code != http.StatusOK || body["imported"].(float64) != 2 {
		t.Fatalf("建订阅应导入 2 个节点：%d %+v", rec.Code, body)
	}
	subID, _ := body["id"].(string)

	_, list := doJSONReq(t, f.pn, http.MethodGet, "/panel/api/proxies", nil)
	subs, _ := list["subs"].([]any)
	if len(subs) != 1 {
		t.Fatalf("应有一条订阅，得到 %+v", list["subs"])
	}
	if got := subs[0].(map[string]any)["count"].(float64); got != 2 {
		t.Fatalf("订阅节点数应为 2，得到 %v", got)
	}

	// 再同步一次：节点已存在 → 不重复入池。
	rec, body = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/subscriptions/"+subID+"/sync", nil)
	if rec.Code != http.StatusOK || body["imported"].(float64) != 0 {
		t.Fatalf("重复同步应 imported=0：%d %+v", rec.Code, body)
	}

	rec, _ = doJSONReq(t, f.pn, http.MethodPost, "/panel/api/subscriptions/none/sync", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("同步不存在订阅应 400，得到 %d", rec.Code)
	}

	rec, _ = doJSONReq(t, f.pn, http.MethodDelete, "/panel/api/subscriptions/"+subID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删订阅应 200，得到 %d", rec.Code)
	}
	rec, _ = doJSONReq(t, f.pn, http.MethodDelete, "/panel/api/subscriptions/"+subID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删订阅应 404，得到 %d", rec.Code)
	}
}

// TestProxySubscriptionBadURL 非 http(s) 订阅地址与拉取失败都要 400 且不留下空订阅。
func TestProxySubscriptionBadURL(t *testing.T) {
	f := newProxyFixture(t)
	rec, _ := doJSONReq(t, f.pn, http.MethodPost, "/panel/api/subscriptions",
		map[string]any{"url": "socks5://127.0.0.1:1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非 http(s) 订阅应 400，得到 %d", rec.Code)
	}
	if got := len(f.br.Subs()); got != 0 {
		t.Fatalf("失败不应留下订阅记录，得到 %d 条", got)
	}
}

// TestProxyEndpointsWithoutPool 未装配代理池 → 501（不是 panic）。
func TestProxyEndpointsWithoutPool(t *testing.T) {
	p := pool.New("")
	pn := New(Config{Version: "test", Pool: p})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/panel/api/proxies"},
		{http.MethodPost, "/panel/api/proxies"},
		{http.MethodPost, "/panel/api/proxies/x/toggle"},
		{http.MethodPost, "/panel/api/subscriptions"},
	} {
		rec, _ := doJSONReq(t, pn, tc.method, tc.path, map[string]any{})
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s 应 501，得到 %d", tc.method, tc.path, rec.Code)
		}
	}
}
