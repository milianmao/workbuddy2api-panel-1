package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// autoBindRaw 造一个 socks5 节点定义（端口只占位，测试里不会被真拨）。
func autoBindRaw(name string, port int) map[string]any {
	return map[string]any{"name": name, "type": "socks5", "server": "127.0.0.1", "port": port}
}

// newAutoBindFixture 四个节点 + 面板；cap 由 livecfg 注入。
func newAutoBindFixture(t *testing.T, off bool, cap int) *proxyFixture {
	t.Helper()
	f := newProxyFixtureWith(t,
		autoBindRaw("A", 1), autoBindRaw("B", 2), autoBindRaw("C", 3), autoBindRaw("D", 4),
	)
	f.pn.cfg.Live = livecfg.New(livecfg.Snapshot{
		ImportAutoBindOff:         off,
		ImportAutoBindMaxPerProxy: cap,
	})
	return f
}

// newAccts 造 n 个「刚导入、池里没有」的账号。
func newAccts(prefix string, n int) []importAcct {
	out := make([]importAcct, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, importAcct{uid: fmt.Sprintf("%s-%d", prefix, i), isNew: true})
	}
	return out
}

// fixedCounts 节点 ID → 当前 fixed 绑定数。
func fixedCounts(snap proxypool.Snapshot) map[string]int {
	out := map[string]int{}
	for _, b := range snap.Bindings {
		if b.Mode == "fixed" {
			out[b.ProxyID]++
		}
	}
	return out
}

// TestImportAutoBindSpreadsWithinCap 一批号必须摊开，且任一节点不超过上限。
//
// 这是本功能的核心不变量：43 个节点只对应约 30 个 distinct 出口 IP，把一批新号全压在
// 前几个节点上，等于把它们一起交给同一批出口去进风控。
func TestImportAutoBindSpreadsWithinCap(t *testing.T) {
	f := newAutoBindFixture(t, false, 2)
	res := f.pn.autoBindImported(newAccts("u", 5))

	if !res.Enabled || res.Cap != 2 {
		t.Fatalf("设置不对: enabled=%v cap=%d", res.Enabled, res.Cap)
	}
	if res.Bound != 5 || res.Skipped != 0 {
		t.Fatalf("应全部绑上: bound=%d skipped=%d reason=%q", res.Bound, res.Skipped, res.Reason)
	}
	counts := fixedCounts(f.pp.Snapshot())
	used := 0
	for id, n := range counts {
		if n > 2 {
			t.Fatalf("节点 %s 绑了 %d 个，超过上限 2", id, n)
		}
		used++
	}
	if used < 3 {
		t.Fatalf("5 个号应该摊到至少 3 个节点，实际只用了 %d 个: %v", used, counts)
	}
	if len(res.Nodes) != used {
		t.Fatalf("结果里的节点分布与实际不一致: %v vs %v", res.Nodes, counts)
	}
}

// TestImportAutoBindStopsAtCap 容量用尽后不再超发：没绑上的号留在池里走默认出口。
func TestImportAutoBindStopsAtCap(t *testing.T) {
	f := newAutoBindFixture(t, false, 1)
	res := f.pn.autoBindImported(newAccts("u", 6))

	if res.Bound != 4 || res.Skipped != 2 {
		t.Fatalf("4 节点 × 上限 1 → 应 bind 4 / skip 2，实际 %d / %d", res.Bound, res.Skipped)
	}
	if res.Reason == "" {
		t.Fatal("有号没绑上时必须给出原因")
	}
	counts := fixedCounts(f.pp.Snapshot())
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("节点 %s 绑了 %d 个，上限是 1", id, n)
		}
	}
	if len(counts) != 4 {
		t.Fatalf("应恰好占满 4 个节点，实际 %d: %v", len(counts), counts)
	}
}

// TestImportAutoBindSkipsFullNodes 已经满额的节点一个都不该分到新号（哪怕它是「最少」）。
func TestImportAutoBindSkipsFullNodes(t *testing.T) {
	f := newAutoBindFixture(t, false, 2)
	// 把 A、B 各绑满 2 个。
	for _, uid := range []string{"old-a1", "old-a2"} {
		if err := f.pp.Bind(uid, proxypool.Binding{Mode: "fixed", ProxyID: f.nodeID(t, "A")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, uid := range []string{"old-b1", "old-b2"} {
		if err := f.pp.Bind(uid, proxypool.Binding{Mode: "fixed", ProxyID: f.nodeID(t, "B")}); err != nil {
			t.Fatal(err)
		}
	}
	res := f.pn.autoBindImported(newAccts("u", 2))
	if res.Bound != 2 {
		t.Fatalf("C/D 还有空位，应 bind 2，实际 %d（reason=%q）", res.Bound, res.Reason)
	}
	for uid, b := range f.pp.Snapshot().Bindings {
		if strings.HasPrefix(uid, "u-") {
			if b.ProxyID == f.nodeID(t, "A") || b.ProxyID == f.nodeID(t, "B") {
				t.Fatalf("新号 %s 被绑到满额节点 %s", uid, b.ProxyID)
			}
		}
	}
}

// TestImportAutoBindKeepsExistingBinding 覆盖导入（号已在池里）不动原有绑定。
func TestImportAutoBindKeepsExistingBinding(t *testing.T) {
	f := newAutoBindFixture(t, false, 2)
	a := f.nodeID(t, "A")
	if err := f.pp.Bind("u1", proxypool.Binding{Mode: "fixed", ProxyID: a}); err != nil {
		t.Fatal(err)
	}
	res := f.pn.autoBindImported([]importAcct{
		{uid: "u1", isNew: false}, // 覆盖导入
		{uid: "u9", isNew: true},
	})
	if res.Bound != 1 {
		t.Fatalf("只该给新号绑一个，实际 %d", res.Bound)
	}
	if got := f.pp.Snapshot().Bindings["u1"]; got.ProxyID != a {
		t.Fatalf("u1 的原绑定被改动了: %+v", got)
	}
	if _, ok := f.pp.Snapshot().Bindings["u9"]; !ok {
		t.Fatal("新号没有绑上")
	}
}

// TestImportAutoBindDisabledBySwitch 开关关掉后一个都不绑，且如实报告。
func TestImportAutoBindDisabledBySwitch(t *testing.T) {
	f := newAutoBindFixture(t, true, 5)
	res := f.pn.autoBindImported(newAccts("u", 3))
	if res.Enabled {
		t.Fatal("开关已关，Enabled 应为 false")
	}
	if res.Bound != 0 || res.Skipped != 3 || res.Reason == "" {
		t.Fatalf("关掉后应 0 绑定并给出原因: %+v", res)
	}
	if n := len(fixedCounts(f.pp.Snapshot())); n != 0 {
		t.Fatalf("关掉后不应产生任何绑定，实际 %d 条", n)
	}
}

// TestImportAutoBindNilProxypool 没接代理池时安全降级（老部署 / 单元测试路径）。
func TestImportAutoBindNilProxypool(t *testing.T) {
	pn := New(Config{Version: "test"})
	res := pn.autoBindImported(newAccts("u", 2))
	if res.Bound != 0 || res.Reason == "" {
		t.Fatalf("没有代理池时应 0 绑定并说明原因: %+v", res)
	}
}

// TestImportCockpitAutoBindsProxy 端到端（面板接口层）：导入成功后响应里带 auto_bind，
// 且绑定真的落进了代理池——这是「接线没接上」唯一能被单元测试挡住的地方。
func TestImportCockpitAutoBindsProxy(t *testing.T) {
	f := newProxyFixtureWith(t, autoBindRaw("A", 1), autoBindRaw("B", 2))
	f.pn.cfg.AuthDir = t.TempDir()
	f.pn.cfg.Upstream = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc2(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusNotFound, `{"code":404,"msg":"not found"}`
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				// 导入会先用 refresh 求证一次；假上游若不认这个端点就会被判 dead/无判词。
				// 这里回成功（含 expiresIn），让落盘走「已验证」这条正常路径。
				status, body = http.StatusOK, importRefreshOK
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", BillingBaseCN: "https://cn.example",
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "a.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(`[` + authFileJSON + `]`)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	f.pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Imported int                  `json:"imported"`
		AutoBind importAutoBindResult `json:"auto_bind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Imported != 1 {
		t.Fatalf("expected 1 imported, got %d: %s", out.Imported, rec.Body.String())
	}
	if !out.AutoBind.Enabled || out.AutoBind.Bound != 1 {
		t.Fatalf("响应里 auto_bind 不正确: %+v（body=%s）", out.AutoBind, rec.Body.String())
	}
	// authFileJSON 的 uid 是 777bd8bc。
	b, ok := f.pp.Snapshot().Bindings["777bd8bc"]
	if !ok || b.Mode != "fixed" || b.ProxyID == "" {
		t.Fatalf("导入后没有落绑定: %+v", f.pp.Snapshot().Bindings)
	}
}

// TestImportCockpitDryRunDoesNotBind 预览绝不写绑定。
func TestImportCockpitDryRunDoesNotBind(t *testing.T) {
	f := newProxyFixtureWith(t, autoBindRaw("A", 1))
	f.pn.cfg.AuthDir = t.TempDir()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.json")
	_, _ = fw.Write([]byte(`[` + authFileJSON + `]`))
	_ = mw.WriteField("dryRun", "true")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	f.pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("预览失败: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(f.pp.Snapshot().Bindings); n != 0 {
		t.Fatalf("预览不应写任何绑定，实际 %d 条", n)
	}
	var out struct {
		AutoBind importAutoBindResult `json:"auto_bind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.AutoBind.DryRun {
		t.Fatalf("预览时 auto_bind 应带 dry_run 标记: %+v", out.AutoBind)
	}
}

// TestImportAutoBindSkipsInfoRows 机场会把套餐文案行一起塞进节点列表，它们拨得通但
// 名字是信息；选点必须跳过（线上曾把一个号绑到「距离下次重置剩余：27 天」上）。
func TestImportAutoBindSkipsInfoRows(t *testing.T) {
	f := newProxyFixtureWith(t,
		autoBindRaw("距离下次重置剩余：27 天", 1),
		autoBindRaw("剩余流量：1.89 TB", 2),
		autoBindRaw("🇸🇬新加坡高速03|BGP|CTCUCM", 3),
	)
	f.pn.cfg.Live = livecfg.New(livecfg.Snapshot{ImportAutoBindMaxPerProxy: 5})

	res := f.pn.autoBindImported(newAccts("u-info", 1))
	if res.Bound != 1 || res.Skipped != 0 {
		t.Fatalf("应绑上 1 个：%+v", res)
	}
	real := f.nodeID(t, "🇸🇬新加坡高速03|BGP|CTCUCM")
	if b := f.pp.Snapshot().Bindings["u-info-0"]; b.ProxyID != real {
		t.Fatalf("应绑到真节点 %s，实际 %+v", real, b)
	}
}
