package proxypool

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// clashYAML 两节点 Clash 订阅（ss + trojan）。
const clashYAML = `proxies:
  - name: 香港节点
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: pass1
  - name: 美国节点
    type: trojan
    server: 5.6.7.8
    port: 443
    password: pass2
    sni: us.example.com
`

// uriList 两个 ss URI（SIP002，明文多行）。
const uriList = "ss://YWVzLTI1Ni1nY206cGFzc3Yy@1.1.1.1:8388#ss-node-a\n" +
	"ss://YWVzLTI1Ni1nY206cGFzc3Yy@2.2.2.2:8388#ss-node-b\n"

// b64URIList uriList 的 base64 包装（机场常见形态）。
var b64URIList = base64.StdEncoding.EncodeToString([]byte(uriList))

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

func TestParseNodesClashYAML(t *testing.T) {
	maps, err := ParseNodes(clashYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(maps) != 2 {
		t.Fatalf("n=%d want 2", len(maps))
	}
	if maps[0]["name"] != "香港节点" || maps[0]["type"] != "ss" {
		t.Fatalf("map[0]=%v", maps[0])
	}
	if _, ok := maps[1]["sni"].(string); !ok {
		t.Fatalf("trojan sni lost: %v", maps[1])
	}
}

func TestParseNodesBase64URIList(t *testing.T) {
	for name, text := range map[string]string{"b64": b64URIList, "plain": uriList} {
		maps, err := ParseNodes(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(maps) != 2 {
			t.Fatalf("%s: n=%d want 2", name, len(maps))
		}
		if maps[0]["name"] != "ss-node-a" || maps[1]["name"] != "ss-node-b" {
			t.Fatalf("%s: names=%v/%v", name, maps[0]["name"], maps[1]["name"])
		}
		// URI 路径 port 为 string，buildNode 需能归一化（validateNodes 侧验证）。
		if got := mapInt(maps[0], "port"); got != 8388 {
			t.Fatalf("%s: port=%d", name, got)
		}
	}
}

func TestParseNodesEmpty(t *testing.T) {
	if _, err := ParseNodes("   \n "); err == nil {
		t.Fatal("empty text should error")
	}
}

// ---------------------------------------------------------------------------
// 导入：tag 去重 / 整体替换 / 手动导入
// ---------------------------------------------------------------------------

func newTestPool(t *testing.T) (*Pool, string) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "proxies.json")
	p := New(fp, nil)
	t.Cleanup(p.Close)
	return p, fp
}

func TestAddRawURIList(t *testing.T) {
	p, _ := newTestPool(t)
	res, err := p.AddRaw(uriList)
	if err != nil {
		t.Fatalf("AddRaw: %v", err)
	}
	if res.Total != 2 || res.Imported != 2 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	nodes := p.List()
	if len(nodes) != 2 {
		t.Fatalf("n=%d", len(nodes))
	}
	for _, n := range nodes {
		if n.SubID != "" {
			t.Fatalf("manual node sub_id=%q want empty", n.SubID)
		}
		if !n.Enabled {
			t.Fatalf("node %s not enabled by default", n.Tag)
		}
		if n.Port != 8388 || n.Type != "ss" || n.Server == "" {
			t.Fatalf("node=%+v", n)
		}
	}
	// tag 唯一：重名加 -2。
	res2, err := p.AddRaw(uriList)
	if err != nil {
		t.Fatalf("AddRaw 2: %v", err)
	}
	if res2.Imported != 2 {
		t.Fatalf("res2=%+v", res2)
	}
	tags := map[string]bool{}
	for _, n := range p.List() {
		tags[n.Tag] = true
	}
	for _, want := range []string{"ss-node-a", "ss-node-b", "ss-node-a-2", "ss-node-b-2"} {
		if !tags[want] {
			t.Fatalf("tag %q missing: %v", want, tags)
		}
	}
}

func TestAddRawInvalidNodeSkipped(t *testing.T) {
	p, _ := newTestPool(t)
	text := "ss://YWVzLTI1Ni1nY206cGFzc3Yy@1.1.1.1:8388#good\n" +
		"not-a-uri-line\n"
	res, err := p.AddRaw(text)
	if err != nil {
		t.Fatalf("AddRaw: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("res=%+v", res)
	}
}

// TestSyncTagDedupAndReplace 整体替换语义：二次同步后旧节点消失、同名节点稳定
// 复用同一 ID、重名节点加后缀。
func TestSyncTagDedupAndReplace(t *testing.T) {
	var unboundIDs []string
	var mu sync.Mutex
	subBody := clashYAML
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != syncUA {
			t.Errorf("subscription UA=%q want %q", ua, syncUA)
		}
		_, _ = io.WriteString(w, subBody)
	}))
	defer srv.Close()

	p, _ := newTestPool(t)
	p.SetUnbindCallback(func(ids []string) []string {
		mu.Lock()
		unboundIDs = append(unboundIDs, ids...)
		mu.Unlock()
		return nil
	})
	sub, err := p.AddSubscription("机场A", srv.URL)
	if err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	res, err := p.Sync(context.Background(), sub.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Total != 2 || res.Imported != 2 {
		t.Fatalf("res1=%+v", res)
	}
	subs := p.Subs()
	if len(subs) != 1 || subs[0].Count != 2 {
		t.Fatalf("subs=%+v", subs)
	}
	byTag := func() map[string]Node {
		m := map[string]Node{}
		for _, n := range p.List() {
			m[n.Tag] = n
		}
		return m
	}
	first := byTag()

	// 二次同步：同一订阅内容 → 整体替换（同 tag 同 id，不产生 -2 后缀）。
	res2, err := p.Sync(context.Background(), sub.ID)
	if err != nil {
		t.Fatalf("Sync2: %v", err)
	}
	if res2.Imported != 2 {
		t.Fatalf("res2=%+v（重导入不应产生重名后缀）", res2)
	}
	second := byTag()
	for tag, id := range map[string]string{"香港节点": first["香港节点"].ID, "美国节点": first["美国节点"].ID} {
		if second[tag].ID != id {
			t.Fatalf("tag %q id changed %s → %s", tag, id, second[tag].ID)
		}
	}

	// 内容变化：机场侧删掉「香港节点」、给「美国节点」换了服务器 → 旧节点删除 +
	// 同名节点 ID 稳定（ID 源自 tag）但 Raw 已替换；新节点照常入池。
	subBody = `proxies:
  - name: 美国节点
    type: ss
    server: 9.9.9.9
    port: 8388
    cipher: aes-256-gcm
    password: p
  - name: 新节点
    type: ss
    server: 8.8.8.8
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	res3, err := p.Sync(context.Background(), sub.ID)
	if err != nil {
		t.Fatalf("Sync3: %v", err)
	}
	if res3.Imported != 2 || res3.Skipped != 0 {
		t.Fatalf("res3=%+v", res3)
	}
	third := byTag()
	if _, ok := third["香港节点"]; ok {
		t.Fatal("旧节点未被整体替换删除")
	}
	// 同 tag → 同 ID（ID 源自 tag，稳定标识语义）。
	if third["美国节点"].ID != first["美国节点"].ID {
		t.Fatalf("同 tag 节点 ID 应稳定: %s → %s", first["美国节点"].ID, third["美国节点"].ID)
	}
	if third["美国节点"].Server != "9.9.9.9" {
		t.Fatalf("同 tag 节点 Raw 未被替换: %+v", third["美国节点"])
	}
	if _, ok := third["新节点"]; !ok {
		t.Fatalf("新节点缺失: %v", third)
	}
	// 被删节点的账号自动解绑回调：至少包含「香港节点」。
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, id := range unboundIDs {
		if id == first["香港节点"].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("被删节点未触发解绑回调: %v", unboundIDs)
	}
	if subs := p.Subs(); subs[0].Count != 2 {
		t.Fatalf("sub count=%d", subs[0].Count)
	}
}

func TestAddSubscriptionIdempotent(t *testing.T) {
	p, _ := newTestPool(t)
	s1, err := p.AddSubscription("n1", "https://sub.example/a")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := p.AddSubscription("n2", "https://sub.example/a")
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != s2.ID {
		t.Fatalf("same url should dedupe: %s vs %s", s1.ID, s2.ID)
	}
	if _, err := p.AddSubscription("", ""); err == nil {
		t.Fatal("empty url should error")
	}
}

func TestSyncUnknownSub(t *testing.T) {
	p, _ := newTestPool(t)
	if _, err := p.Sync(context.Background(), "nope"); err == nil {
		t.Fatal("unknown sub should error")
	}
}

// TestSyncEmptyBodyKeepsNodes 机场错发登录页/限流页（一个节点都解析不出）时按失败
// 处理，不执行整体替换——防止一次坏响应清空该订阅全部节点。
func TestSyncEmptyBodyKeepsNodes(t *testing.T) {
	subBody := "<html>please login</html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subBody)
	}))
	defer srv.Close()
	p, _ := newTestPool(t)
	res, err := p.AddRaw(uriList)
	if err != nil || res.Imported != 2 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	sub, _ := p.AddSubscription("机场", srv.URL)
	if _, err := p.Sync(context.Background(), sub.ID); err == nil {
		t.Fatal("empty parse should error")
	}
	if got := len(p.List()); got != 2 {
		t.Fatalf("nodes were wiped: %d", got)
	}
}

func TestDeleteSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, clashYAML)
	}))
	defer srv.Close()
	p, _ := newTestPool(t)
	sub, _ := p.AddSubscription("机场", srv.URL)
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	removed, _, err := p.DeleteSubscription(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v", removed)
	}
	if nodes := p.List(); len(nodes) != 0 {
		t.Fatalf("nodes left: %d", len(nodes))
	}
	if _, _, err := p.DeleteSubscription(sub.ID); err == nil {
		t.Fatal("double delete should 404")
	}
}

// ---------------------------------------------------------------------------
// 持久化 roundtrip
// ---------------------------------------------------------------------------

func TestPersistRoundtrip(t *testing.T) {
	p, fp := newTestPool(t)
	res, err := p.AddRaw(uriList)
	if err != nil || res.Imported != 2 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	id := p.List()[0].ID
	if enabled, err := p.Toggle(id); err != nil || enabled {
		t.Fatalf("Toggle=%v err=%v", enabled, err)
	}
	p.Flush()

	p2 := New(fp, nil)
	defer p2.Close()
	nodes := p2.List()
	if len(nodes) != 2 {
		t.Fatalf("reload n=%d", len(nodes))
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	n, ok := byID[id]
	if !ok {
		t.Fatalf("id %s lost after reload", id)
	}
	if n.Enabled {
		t.Fatal("enabled 状态未持久化")
	}
	if n.Tag != p.List()[0].Tag {
		t.Fatalf("tag mismatch %s vs %s", n.Tag, p.List()[0].Tag)
	}
	if n.Raw["type"] != "ss" {
		t.Fatalf("raw lost: %v", n.Raw)
	}
}

// ---------------------------------------------------------------------------
// TransportFor
// ---------------------------------------------------------------------------

func TestTransportForNilCases(t *testing.T) {
	p, _ := newTestPool(t)
	if tr := p.TransportFor(""); tr != nil {
		t.Fatal("empty id should be nil")
	}
	if tr := p.TransportFor("deadbeef"); tr != nil {
		t.Fatal("unknown id should be nil")
	}
	res, _ := p.AddRaw(uriList)
	if res.Imported != 2 {
		t.Fatalf("res=%+v", res)
	}
	id := p.List()[0].ID
	if _, err := p.Toggle(id); err != nil {
		t.Fatal(err)
	}
	if tr := p.TransportFor(id); tr != nil {
		t.Fatal("disabled node should be nil")
	}
	// 再切回来，有底座才能建 transport；无底座（base=nil）恒 nil。
	if _, err := p.Toggle(id); err != nil {
		t.Fatal(err)
	}
	if tr := p.TransportFor(id); tr != nil {
		t.Fatal("base nil should be nil")
	}
}

// fakeCONNECTProxy 只处理 CONNECT 的本地假 HTTP 代理：拨向 CONNECT 目标后双向转发。
// connectCount 记录隧道建立次数（验证流量确实走了节点）。
func fakeCONNECTProxy(t *testing.T) (addr string, connectCount *int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				atomic.AddInt64(&count, 1)
				target, err := net.Dial("tcp", req.Host)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer target.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(target, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, target); done <- struct{}{} }()
				<-done
				<-done
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), &count
}

// TestTransportDialsThroughNode 端到端：http 类型节点（本地假 CONNECT 代理）+
// httptest 目标。TransportFor 的 DialContext 必须经节点隧道转发（connectCount>0），
// 节点停用后 transport 恒 nil（直连回落由 upstream 侧负责）。
func TestTransportDialsThroughNode(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	proxyAddr, connects := fakeCONNECTProxy(t)

	base := func() *http.Transport { return &http.Transport{} }
	p := New("", base)
	defer p.Close()
	res, err := p.AddRaw(fmt.Sprintf("http://u:p@%s#local-http", proxyAddr))
	if err != nil || res.Imported != 1 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	if got := p.List()[0].Type; got != "http" {
		t.Fatalf("type=%q want http", got)
	}
	id := p.List()[0].ID
	tr := p.TransportFor(id)
	if tr == nil {
		t.Fatal("TransportFor nil with base injected")
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET via node: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if atomic.LoadInt64(connects) == 0 {
		t.Fatal("流量未经过节点（CONNECT 计数为 0）")
	}
	// 同一 transport 被缓存（指针稳定）。
	if tr2 := p.TransportFor(id); tr2 != tr {
		t.Fatal("transport 未按节点缓存")
	}
}

// ---------------------------------------------------------------------------
// 延迟测试
// ---------------------------------------------------------------------------

// TestTestAndBatch 端到端延迟测试：URLTest 经节点隧道打本地 204 目标。
func TestTestAndBatch(t *testing.T) {
	oldURL, oldTimeout := latencyTestURL, latencyTimeout
	latencyTestURL = "http://replace-in-test/generate_204"
	defer func() { latencyTestURL, latencyTimeout = oldURL, oldTimeout }()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	proxyAddr, _ := fakeCONNECTProxy(t)
	p, _ := newTestPool(t)
	res, _ := p.AddRaw(fmt.Sprintf("http://u:p@%s#lat-node\nss://YWVzLTI1Ni1nY206cGFzcw@10.255.255.1:8388#dead-node", proxyAddr))
	if res.Imported != 2 {
		t.Fatalf("res=%+v", res)
	}
	nodes := p.List()
	var liveID, deadID string
	for _, n := range nodes {
		if n.Tag == "lat-node" {
			liveID = n.ID
		} else {
			deadID = n.ID
		}
	}
	latencyTestURL = target.URL
	latencyTimeout = 3 * time.Second

	ms, err := p.Test(context.Background(), liveID)
	if err != nil {
		t.Fatalf("Test live: %v", err)
	}
	if ms <= 0 {
		t.Fatalf("latency=%d", ms)
	}
	var live Node
	for _, n := range p.List() {
		if n.ID == liveID {
			live = n
		}
	}
	if live.ID == "" || live.LatencyMS != ms || live.Fails != 0 || live.CheckedAt == 0 {
		t.Fatalf("观测未回写: %+v", live)
	}

	// 死节点（不可路由地址）：失败 → Fails++ 且 LatencyMS 归零。
	if _, err := p.Test(context.Background(), deadID); err == nil {
		t.Fatal("dead node should fail")
	}
	for _, n := range p.List() {
		if n.ID == deadID && n.Fails != 1 {
			t.Fatalf("dead fails=%d want 1", n.Fails)
		}
	}

	// 批量：顺序与入参一致；未知 id 落 error。
	results := p.TestBatch(context.Background(), []string{liveID, "nope", deadID})
	if len(results) != 3 || results[0].ID != liveID || results[1].ID != "nope" || results[2].ID != deadID {
		t.Fatalf("results=%+v", results)
	}
	if !results[0].OK || results[0].LatencyMS <= 0 {
		t.Fatalf("live result=%+v", results[0])
	}
	if results[1].OK || results[1].Error == "" {
		t.Fatalf("unknown result=%+v", results[1])
	}
	if results[2].OK || results[2].Error == "" {
		t.Fatalf("dead result=%+v", results[2])
	}
}

func TestTestUnknownNode(t *testing.T) {
	p, _ := newTestPool(t)
	if _, err := p.Test(context.Background(), "missing"); err == nil {
		t.Fatal("unknown id should error")
	}
}

// TestTogglePersistsAfterReload 启停状态与 transport 缓存联动（含重载）。
func TestTogglePersistsAfterReload(t *testing.T) {
	p, fp := newTestPool(t)
	res, _ := p.AddRaw(uriList)
	id := p.List()[0].ID
	_ = res
	if _, err := p.Toggle(id); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p2 := New(fp, nil)
	defer p2.Close()
	for _, n := range p2.List() {
		if n.ID == id && n.Enabled {
			t.Fatal("disabled state lost after reload")
		}
	}
}

// ---------------------------------------------------------------------------
// B1 回归：同步只解绑**真正消失**的节点（内容不变 → 绑定必须保留）
// ---------------------------------------------------------------------------

// newCountingSubServer 订阅源：body 可换，记录拉取次数。
func newCountingSubServer(t *testing.T, body *string, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		_, _ = io.WriteString(w, *body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResyncKeepsBindingsWhenContentUnchanged B1：同一份订阅内容重复同步，节点 ID
// 不变 → 不得触发任何解绑（修复前对全部旧 ID 回调解绑，账号全部掉绑定）。
func TestResyncKeepsBindingsWhenContentUnchanged(t *testing.T) {
	body := clashYAML
	var hits int64
	srv := newCountingSubServer(t, &body, &hits)

	p, _ := newTestPool(t)
	var unboundMu sync.Mutex
	var unbound []string
	p.SetUnbindCallback(func(ids []string) []string {
		unboundMu.Lock()
		unbound = append(unbound, ids...)
		unboundMu.Unlock()
		return nil
	})
	sub, err := p.AddSubscription("机场", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("Sync1: %v", err)
	}
	before := p.List()
	if len(before) != 2 {
		t.Fatalf("n=%d", len(before))
	}

	res, err := p.Sync(context.Background(), sub.ID)
	if err != nil {
		t.Fatalf("Sync2: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("res=%+v", res)
	}
	unboundMu.Lock()
	got := append([]string(nil), unbound...)
	unboundMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("内容未变却解绑了节点: %v（绑定账号会被清空）", got)
	}
	after := p.List()
	if len(after) != 2 {
		t.Fatalf("after n=%d", len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Tag != after[i].Tag {
			t.Fatalf("节点集合应稳定: %+v → %+v", before[i], after[i])
		}
	}
}

// TestResyncContentChangeKeepsBindingAndSwapsTransport B1 之二：同 tag 换服务器 →
// ID 不变（绑定保留、不解绑），但 transport 必须换新实例（旧连接指向旧服务器）。
func TestResyncContentChangeKeepsBindingAndSwapsTransport(t *testing.T) {
	body := `proxies:
  - name: 香港节点
    type: http
    server: 127.0.0.1
    port: 8080
    username: u
    password: p
`
	var hits int64
	srv := newCountingSubServer(t, &body, &hits)

	base := func() *http.Transport { return &http.Transport{} }
	p := New("", base)
	defer p.Close()
	var unboundMu sync.Mutex
	var unbound []string
	p.SetUnbindCallback(func(ids []string) []string {
		unboundMu.Lock()
		unbound = append(unbound, ids...)
		unboundMu.Unlock()
		return nil
	})
	sub, err := p.AddSubscription("机场", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("Sync1: %v", err)
	}
	n := p.List()[0]
	id := n.ID
	tr1 := p.TransportFor(id)
	if tr1 == nil {
		t.Fatal("TransportFor nil")
	}

	// 同 tag，换端口（内容变更）。
	body = `proxies:
  - name: 香港节点
    type: http
    server: 127.0.0.1
    port: 9090
    username: u
    password: p
`
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("Sync2: %v", err)
	}
	nodes := p.List()
	if len(nodes) != 1 || nodes[0].ID != id {
		t.Fatalf("同 tag 应保持同一 ID: %+v (want %s)", nodes, id)
	}
	if nodes[0].Port != 9090 {
		t.Fatalf("内容未替换: %+v", nodes[0])
	}
	unboundMu.Lock()
	got := append([]string(nil), unbound...)
	unboundMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("同 tag 节点 ID 未变，不该解绑: %v", got)
	}
	tr2 := p.TransportFor(id)
	if tr2 == nil {
		t.Fatal("TransportFor nil after resync")
	}
	if tr2 == tr1 {
		t.Fatal("节点内容已变，transport 必须换新实例（旧连接指向旧服务器）")
	}
}

// TestResyncSameContentKeepsTransportInstance 内容**没变**时不该无谓重建 transport
// （每次同步都清空连接池是纯损耗）。
func TestResyncSameContentKeepsTransportInstance(t *testing.T) {
	body := clashYAML
	var hits int64
	srv := newCountingSubServer(t, &body, &hits)
	base := func() *http.Transport { return &http.Transport{} }
	p := New("", base)
	defer p.Close()
	sub, _ := p.AddSubscription("机场", srv.URL)
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("Sync1: %v", err)
	}
	id := p.List()[0].ID
	tr1 := p.TransportFor(id)
	if tr1 == nil {
		t.Fatal("nil transport")
	}
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("Sync2: %v", err)
	}
	if tr2 := p.TransportFor(id); tr2 != tr1 {
		t.Fatal("内容未变的节点不应重建 transport")
	}
}

// TestResyncRekeyedNodeUnbindsOldID 相邻缺陷：另一订阅释放同名 tag 后本订阅节点
// 重新键控（ID 变）→ 旧 ID 必须并入解绑差集，不留悬空绑定。
//
// 构造：订阅 A 先拿到 tag "shared"，订阅 B 的同一 tag 被挤成 "shared-2"；随后订阅 A
// 被删除（释放 "shared"），B 重同步 → tag 回到 "shared" → ID 变化。
func TestResyncRekeyedNodeUnbindsOldID(t *testing.T) {
	shared := `proxies:
  - name: shared
    type: ss
    server: 1.1.1.1
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	var hitsA, hitsB int64
	srvA := newCountingSubServer(t, &shared, &hitsA)
	srvB := newCountingSubServer(t, &shared, &hitsB)

	p, _ := newTestPool(t)
	var unboundMu sync.Mutex
	var unbound []string
	p.SetUnbindCallback(func(ids []string) []string {
		unboundMu.Lock()
		unbound = append(unbound, ids...)
		unboundMu.Unlock()
		return nil
	})
	subA, _ := p.AddSubscription("A", srvA.URL)
	subB, _ := p.AddSubscription("B", srvB.URL)
	if _, err := p.Sync(context.Background(), subA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sync(context.Background(), subB.ID); err != nil {
		t.Fatal(err)
	}
	var idB string
	for _, n := range p.List() {
		if n.SubID == subB.ID {
			idB = n.ID
			if n.Tag != "shared-2" {
				t.Fatalf("B 的节点应被挤成 shared-2，得到 %q", n.Tag)
			}
		}
	}
	if idB == "" {
		t.Fatal("B 节点缺失")
	}

	// 删除订阅 A 释放 "shared"（A 的节点只属 A，B 的节点不受影响）。
	if _, _, err := p.DeleteSubscription(subA.ID); err != nil {
		t.Fatal(err)
	}
	unboundMu.Lock()
	unbound = nil
	unboundMu.Unlock()

	if _, err := p.Sync(context.Background(), subB.ID); err != nil {
		t.Fatalf("Sync B: %v", err)
	}
	var nodeB *Node
	for i, n := range p.List() {
		if n.SubID == subB.ID {
			nodeB = &p.List()[i]
		}
	}
	if nodeB == nil {
		t.Fatal("B 节点在重同步后消失")
	}
	if nodeB.Tag != "shared" {
		t.Fatalf("B 应重新拿到 tag shared，得到 %q", nodeB.Tag)
	}
	if nodeB.ID == idB {
		t.Fatalf("tag 变更应导致 ID 重键: %s", idB)
	}
	unboundMu.Lock()
	got := append([]string(nil), unbound...)
	unboundMu.Unlock()
	found := false
	for _, id := range got {
		if id == idB {
			found = true
		}
	}
	if !found {
		t.Fatalf("重键后的旧 ID %s 必须并入解绑差集（否则绑定悬空 → TransportFor 静默 nil）: %v", idB, got)
	}
	if p.TransportFor(idB) != nil {
		t.Fatal("旧 ID 不应还能拿到 transport")
	}
}

// ---------------------------------------------------------------------------
// S2 回归：解析出节点但全部校验失败 → 拒绝替换
// ---------------------------------------------------------------------------

func TestSyncAllInvalidKeepsNodesAndLastSync(t *testing.T) {
	body := clashYAML
	var hits int64
	srv := newCountingSubServer(t, &body, &hits)
	p, _ := newTestPool(t)
	var unboundMu sync.Mutex
	var unbound []string
	p.SetUnbindCallback(func(ids []string) []string {
		unboundMu.Lock()
		unbound = append(unbound, ids...)
		unboundMu.Unlock()
		return nil
	})
	sub, _ := p.AddSubscription("机场", srv.URL)
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	before := p.List()
	lastSync := p.Subs()[0].LastSyncAt
	if lastSync == 0 {
		t.Fatal("首次同步应写 LastSyncAt")
	}
	unboundMu.Lock()
	unbound = nil
	unboundMu.Unlock()

	// 机场改成一个解析得出但 mihomo 不支持的节点类型。
	body = `proxies:
  - name: bad-node
    type: not-a-real-protocol
    server: 1.2.3.4
    port: 1234
`
	res, err := p.Sync(context.Background(), sub.ID)
	if err == nil {
		t.Fatalf("全部校验失败应返回错误，得到 %+v", res)
	}
	if res.Imported != 0 {
		t.Fatalf("res=%+v", res)
	}
	after := p.List()
	if len(after) != len(before) {
		t.Fatalf("节点被清空: %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Fatalf("节点集合应原样保留: %+v → %+v", before, after)
		}
	}
	if got := p.Subs()[0].LastSyncAt; got != lastSync {
		t.Fatalf("同步失败不该更新 LastSyncAt: %d → %d", lastSync, got)
	}
	unboundMu.Lock()
	got := append([]string(nil), unbound...)
	unboundMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("同步失败不该解绑: %v", got)
	}
}

// ---------------------------------------------------------------------------
// S3 回归：重同步继承 Enabled 与延迟观测
// ---------------------------------------------------------------------------

func TestResyncInheritsOperationalState(t *testing.T) {
	body := clashYAML
	var hits int64
	srv := newCountingSubServer(t, &body, &hits)
	p, _ := newTestPool(t)
	sub, _ := p.AddSubscription("机场", srv.URL)
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	// 挑一个节点：手动停用 + 写一份延迟观测。
	target := p.List()[0]
	if _, err := p.Toggle(target.ID); err != nil {
		t.Fatal(err)
	}
	p.noteTestOK(target.ID, 137)
	// 另一个节点：只写观测（保持启用），验证观测也继承。
	other := p.List()[1]
	p.noteTestOK(other.ID, 88)
	if _, err := p.Toggle(other.ID); err != nil { // 停用再启用，回到 enabled
		t.Fatal(err)
	}
	wantEnabled := map[string]bool{}
	wantLatency := map[string]int{}
	wantCheckedAt := map[string]int64{}
	for _, n := range p.List() {
		wantEnabled[n.ID] = n.Enabled
		wantLatency[n.ID] = n.LatencyMS
		wantCheckedAt[n.ID] = n.CheckedAt
	}

	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("resync: %v", err)
	}
	for _, n := range p.List() {
		if n.Enabled != wantEnabled[n.ID] {
			t.Fatalf("节点 %s 的 Enabled 被重同步重置: %v → %v", n.Tag, wantEnabled[n.ID], n.Enabled)
		}
		if n.LatencyMS != wantLatency[n.ID] || n.CheckedAt != wantCheckedAt[n.ID] {
			t.Fatalf("节点 %s 的延迟观测被清零: %d/%d → %d/%d",
				n.Tag, wantLatency[n.ID], wantCheckedAt[n.ID], n.LatencyMS, n.CheckedAt)
		}
	}
	// 新出现的节点默认启用。
	body = clashYAML + `  - name: 新节点
    type: ss
    server: 7.7.7.7
    port: 8388
    cipher: aes-256-gcm
    password: p
`
	if _, err := p.Sync(context.Background(), sub.ID); err != nil {
		t.Fatalf("resync2: %v", err)
	}
	for _, n := range p.List() {
		if n.Tag == "新节点" && !n.Enabled {
			t.Fatal("新节点应默认启用")
		}
	}
}

// ---------------------------------------------------------------------------
// S1 回归：节点拨号有建连上限（黑洞地址不能卡到系统 TCP 重传窗口）
// ---------------------------------------------------------------------------

// TestDialViaNodeRespectsTimeout 黑洞地址（192.0.2.0/24，TEST-NET-1）拨号必须在
// 注入的建连上限内失败。修复前 DialContext 整体替换后 mihomo 内部 dialer 无超时，
// 实测 21s 才返回（本机实测值），远大于断言阈值。
func TestDialViaNodeRespectsTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("dial timeout test skipped in -short")
	}
	dialBlackhole := func(t *testing.T, configure func(p *Pool)) time.Duration {
		t.Helper()
		p := New("", func() *http.Transport { return &http.Transport{} })
		defer p.Close()
		if configure != nil {
			configure(p)
		}
		res, err := p.AddRaw("http://u:p@192.0.2.1:8388#blackhole")
		if err != nil || res.Imported != 1 {
			t.Fatalf("AddRaw=%+v err=%v", res, err)
		}
		tr := p.TransportFor(p.List()[0].ID)
		if tr == nil {
			t.Fatal("nil transport")
		}
		// 直接拨 transport 的 DialContext（绕开 http 层，避免请求级超时掩盖问题）。
		start := time.Now()
		conn, err := tr.DialContext(context.Background(), "tcp", "example.com:443")
		elapsed := time.Since(start)
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("黑洞地址不应拨通")
		}
		return elapsed
	}

	// 生产默认口径（10s）：必须远早于系统 TCP 重传窗口（本机实测 21s）失败。
	t.Run("default", func(t *testing.T) {
		if elapsed := dialBlackhole(t, nil); elapsed > 15*time.Second {
			t.Fatalf("默认口径下建连未在 15s 内失败：耗时 %v（修复前 ~21s）", elapsed)
		}
	})
	// 注入更短的口径：验证超时来自 context 而非环境。
	t.Run("injected", func(t *testing.T) {
		elapsed := dialBlackhole(t, func(p *Pool) { p.SetNodeDialTimeout(700 * time.Millisecond) })
		if elapsed > 5*time.Second {
			t.Fatalf("建连未在注入上限（700ms）附近失败：耗时 %v", elapsed)
		}
	})
}

// TestDefaultNodeDialTimeoutMatchesUpstream 默认口径必须与共享 transport 的建连
// 上限同量级（10s）。与 upstream.dialTimeout 的等值断言在 cmd/server 侧
// （那层才允许同时 import 两个包）。
func TestDefaultNodeDialTimeout(t *testing.T) {
	if DefaultNodeDialTimeout != 10*time.Second {
		t.Fatalf("DefaultNodeDialTimeout=%v want 10s", DefaultNodeDialTimeout)
	}
}

// ---------------------------------------------------------------------------
// S4 回归：读盘脏数据（nil 条目 / 缺 enabled / ID 与 tag 不符）
// ---------------------------------------------------------------------------

func TestLoadSanitizesBadEntries(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "proxies.json")
	goodTag := "good-node"
	goodID := tagID(goodTag)
	disabledTag := "off-node"
	disabledID := tagID(disabledTag)
	// 手工构造：null 节点、缺 enabled 的节点、显式 enabled=false 的节点、
	// id 与 tag 不匹配的脏条目。
	raw := `{
  "version": 1,
  "subs": {"s1": null},
  "nodes": {
    "abc": null,
    "` + goodID + `": {"id":"` + goodID + `","tag":"` + goodTag + `","type":"ss","server":"1.1.1.1","port":8388,"raw":{"type":"ss"}},
    "` + disabledID + `": {"id":"` + disabledID + `","tag":"` + disabledTag + `","type":"ss","server":"2.2.2.2","port":8388,"enabled":false,"raw":{"type":"ss"}},
    "mismatch": {"id":"mismatch","tag":"other-tag","type":"ss","server":"3.3.3.3","port":8388,"raw":{"type":"ss"}}
  }
}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp, nil)
	defer p.Close()

	nodes := p.List() // 修复前：null 条目在此 panic（nil 解引用）
	if len(nodes) != 2 {
		t.Fatalf("n=%d（null 与 id/tag 不符的条目应被丢弃，得到 %+v）", len(nodes), nodes)
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	good, ok := byID[goodID]
	if !ok {
		t.Fatalf("合法节点丢失: %v", byID)
	}
	if !good.Enabled {
		t.Fatal("缺 enabled 字段应按「缺省启用」补齐（修复前被零值判为停用）")
	}
	off, ok := byID[disabledID]
	if !ok {
		t.Fatal("显式停用的节点丢失")
	}
	if off.Enabled {
		t.Fatal("显式 enabled=false 必须保持停用")
	}
	if _, ok := byID["mismatch"]; ok {
		t.Fatal("id 与 tagID(tag) 不符的脏条目应被丢弃")
	}
	// subs 里的 null 同样不得让 Subs 崩。
	if subs := p.Subs(); len(subs) != 0 {
		t.Fatalf("subs=%+v", subs)
	}
	// 落盘后再读一次：缺省补齐的 enabled 应写成 true（格式兼容、可幂等重读）。
	p.Flush()
	p2 := New(fp, nil)
	defer p2.Close()
	for _, n := range p2.List() {
		if n.ID == goodID && !n.Enabled {
			t.Fatal("补齐后的 enabled 未落盘")
		}
	}
}

// ---------------------------------------------------------------------------
// S5 回归：Close 并发幂等
// ---------------------------------------------------------------------------

// TestCloseConcurrent 并发 Close 不得 panic/数据竞争（-race 下验证）。
func TestCloseConcurrent(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "proxies.json"), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Close()
			p.Close()
		}()
	}
	wg.Wait()
	p.Close() // 串行重复调用同样安全
}

// ---------------------------------------------------------------------------
// S6 回归：ExitKey = 实际出口（三种回落都归直连组）
// ---------------------------------------------------------------------------

func TestExitKeyFallbacks(t *testing.T) {
	p, _ := newTestPool(t)
	if got := p.ExitKey(""); got != "" {
		t.Fatalf("空 id 应归直连组，得到 %q", got)
	}
	if got := p.ExitKey("does-not-exist"); got != "" {
		t.Fatalf("不存在的节点应归直连组，得到 %q", got)
	}
	res, _ := p.AddRaw(uriList)
	if res.Imported != 2 {
		t.Fatalf("res=%+v", res)
	}
	id := p.List()[0].ID
	if got := p.ExitKey(id); got != id {
		t.Fatalf("启用节点应返回自身 id，得到 %q", got)
	}
	if _, err := p.Toggle(id); err != nil {
		t.Fatal(err)
	}
	if got := p.ExitKey(id); got != "" {
		t.Fatalf("停用节点应归直连组（实际出口 = 直连），得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// S10 回归：订阅 URL scheme 校验 / 手动导入节点数上限
// ---------------------------------------------------------------------------

func TestAddSubscriptionRejectsBadScheme(t *testing.T) {
	p, _ := newTestPool(t)
	for _, bad := range []string{
		"ftp://sub.example/a",
		"file:///etc/passwd",
		"sub.example/a", // 无 scheme
		"://nope",
		"https://", // 无 host
	} {
		if _, err := p.AddSubscription("x", bad); err == nil {
			t.Fatalf("AddSubscription(%q) 应报错", bad)
		}
	}
	if len(p.Subs()) != 0 {
		t.Fatalf("非法 URL 不得入池: %+v", p.Subs())
	}
	for _, ok := range []string{"http://sub.example/a", "https://sub.example/a?token=1"} {
		if _, err := p.AddSubscription("x", ok); err != nil {
			t.Fatalf("AddSubscription(%q): %v", ok, err)
		}
	}
}

func TestAddRawNodeLimit(t *testing.T) {
	p, _ := newTestPool(t)
	var sb strings.Builder
	for i := 0; i < maxImportNodes+5; i++ {
		fmt.Fprintf(&sb, "ss://YWVzLTI1Ni1nY206cA@10.0.0.%d:8388#n%d\n", i%250, i)
	}
	res, err := p.AddRaw(sb.String())
	if err != nil {
		t.Fatalf("AddRaw: %v", err)
	}
	if res.Imported != maxImportNodes {
		t.Fatalf("imported=%d want %d", res.Imported, maxImportNodes)
	}
	if res.Total != maxImportNodes+5 {
		t.Fatalf("total=%d", res.Total)
	}
	if res.Skipped != 5 {
		t.Fatalf("skipped=%d want 5", res.Skipped)
	}
	joined := strings.Join(res.Errors, " | ")
	if !strings.Contains(joined, "limit") {
		t.Fatalf("errors 应说明超限: %v", res.Errors)
	}
	if got := len(p.List()); got != maxImportNodes {
		t.Fatalf("池内节点数=%d", got)
	}
}

// TestAddRawEmptyTextErrors 空白/无节点文本应为明确错误（而不是静默成功）。
func TestAddRawEmptyTextErrors(t *testing.T) {
	p, _ := newTestPool(t)
	if _, err := p.AddRaw("not a uri\nnor yaml\n"); err == nil {
		t.Fatal("无节点文本应报错")
	}
}

// ---------------------------------------------------------------------------
// S10 回归：parse 判定按行锚定 / base64 包裹的 Clash YAML
// ---------------------------------------------------------------------------

// TestParseNodesURIWithProxiesInName 明文 URI 的节点名含 "proxies:" 子串时不得被
// 误判为 Clash YAML（修复前 strings.Contains 误判 → yaml.Unmarshal 失败）。
func TestParseNodesURIWithProxiesInName(t *testing.T) {
	text := "ss://YWVzLTI1Ni1nY206cGFzc3Yy@1.1.1.1:8388#my-proxies:list\n" +
		"ss://YWVzLTI1Ni1nY206cGFzc3Yy@2.2.2.2:8388#see proxies: below\n"
	maps, err := ParseNodes(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(maps) != 2 {
		t.Fatalf("n=%d want 2（URI 列表被误判为 YAML？）: %+v", len(maps), maps)
	}
	if maps[0]["type"] != "ss" || maps[0]["server"] != "1.1.1.1" {
		t.Fatalf("maps[0]=%v", maps[0])
	}
}

// TestParseNodesIndentedProxiesNotTopLevel 缩进的 proxies:（嵌套键）不算订阅段。
func TestParseNodesIndentedProxiesNotTopLevel(t *testing.T) {
	text := "ss://YWVzLTI1Ni1nY206cGFzc3Yy@1.1.1.1:8388#a\n  proxies: not-toplevel\n"
	maps, err := ParseNodes(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(maps) != 1 {
		t.Fatalf("n=%d", len(maps))
	}
}

// TestParseNodesBase64WrappedClashYAML 机场常见形态：整套 Clash YAML 被 base64 包裹。
func TestParseNodesBase64WrappedClashYAML(t *testing.T) {
	wrapped := base64.StdEncoding.EncodeToString([]byte(clashYAML))
	// 机场还会按 76 列折行。
	var folded strings.Builder
	for i := 0; i < len(wrapped); i += 76 {
		end := i + 76
		if end > len(wrapped) {
			end = len(wrapped)
		}
		folded.WriteString(wrapped[i:end])
		folded.WriteString("\r\n")
	}
	for name, text := range map[string]string{"plain-b64": wrapped, "folded-b64": folded.String()} {
		maps, err := ParseNodes(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(maps) != 2 {
			t.Fatalf("%s: n=%d want 2", name, len(maps))
		}
		if maps[0]["name"] != "香港节点" || maps[1]["type"] != "trojan" {
			t.Fatalf("%s: maps=%v", name, maps)
		}
	}
}

// TestParseNodesBase64URIListStillWorks base64 URI 列表不能被 YAML 分支抢走。
func TestParseNodesBase64URIListStillWorks(t *testing.T) {
	maps, err := ParseNodes(b64URIList)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(maps) != 2 || maps[0]["name"] != "ss-node-a" {
		t.Fatalf("maps=%v", maps)
	}
}

// TestParseNodesProxiesKeyVariants YAML 键允许冒号前空白 / 行尾注释；嵌套与
// 前缀相同的怪键不得误判。
func TestParseNodesProxiesKeyVariants(t *testing.T) {
	body := "proxies :\n  - name: spaced\n    type: ss\n    server: 1.1.1.1\n    port: 8388\n    cipher: aes-256-gcm\n    password: p\n"
	maps, err := ParseNodes(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(maps) != 1 || maps[0]["name"] != "spaced" {
		t.Fatalf("maps=%v", maps)
	}
	// comments 前后仍按 YAML；"proxies:x" 不是 proxies 键 → 走 URI 路径（解析不出）。
	uri := "ss://YWVzLTI1Ni1nY206cA@1.1.1.1:8388#a\nproxies:not-a-key\n"
	maps, err = ParseNodes(uri)
	if err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	if len(maps) != 1 {
		t.Fatalf("maps=%v", maps)
	}
}
