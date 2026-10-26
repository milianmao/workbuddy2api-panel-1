package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// newTransportCounting 构造一个把所有连接拨向 listener 的自定义 transport，
// 并记录拨号次数（验证流量确实走了注入的 transport 而不是共享 client）。
func newTransportCounting(t *testing.T, ln net.Listener, dials *int64) *http.Transport {
	t.Helper()
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			atomic.AddInt64(dials, 1)
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", ln.Addr().String())
		},
	}
}

// TestClientForFallbacks 未注入 / 闭包返回 nil / a 为 nil 时回落共享 client。
func TestClientForFallbacks(t *testing.T) {
	c := New()
	reg, chat := c.clientFor(&auth.Auth{UID: "u1"})
	if reg != c.HTTP || chat != c.ChatHTTP {
		t.Fatal("未注入 accountTransport 时应回落共享 client")
	}
	// 闭包返回 nil → 直连。
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return nil, "" })
	reg, chat = c.clientFor(&auth.Auth{UID: "u1"})
	if reg != c.HTTP || chat != c.ChatHTTP {
		t.Fatal("闭包返回 nil 应回落共享 client")
	}
	// a 为 nil（无账号上下文的调用）→ 直连。
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return &http.Transport{}, "k" })
	reg, chat = c.clientFor(nil)
	if reg != c.HTTP || chat != c.ChatHTTP {
		t.Fatal("a=nil 应回落共享 client")
	}
}

// TestClientForCachesByTransport 同一 transport 指针复用同一对 client；换 transport
// 换 client（节点更新时旧缓存自然失效的语义基础）。超时口径：pair 逐字段镜像
// 直连 client（c.HTTP.Timeout / c.chatHTTP().Timeout），配置非默认值不被绕开。
func TestClientForCachesByTransport(t *testing.T) {
	c := New()
	tr1 := &http.Transport{}
	tr2 := &http.Transport{}
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return tr1, "node-a" })
	a := &auth.Auth{UID: "u1"}
	// 模拟 main 的配置覆盖：直连路径超时被改成非默认值，代理 pair 必须跟随。
	c.HTTP.Timeout = 45 * time.Second
	r1, c1 := c.clientFor(a)
	r1b, c1b := c.clientFor(a)
	if r1 != r1b || c1 != c1b {
		t.Fatal("同 transport 应命中缓存")
	}
	if r1.Timeout != c.HTTP.Timeout {
		t.Fatalf("regular 应镜像 c.HTTP.Timeout: got %v want %v", r1.Timeout, c.HTTP.Timeout)
	}
	if c1.Timeout != c.chatHTTP().Timeout {
		t.Fatalf("chat 应镜像 c.chatHTTP().Timeout: got %v want %v", c1.Timeout, c.chatHTTP().Timeout)
	}
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return tr2, "node-a" })
	r2, c2 := c.clientFor(a)
	if r2 == r1 || c2 == c1 {
		t.Fatal("换 transport 应换 client")
	}
	if r2.Transport != tr2 || c2.Transport != tr2 {
		t.Fatal("client 应使用注入的 transport")
	}
}

// TestDoJSONViaAccountTransport 端到端：doJSON 路径的流量必须走注入 transport 的
// 拨号器（假域名 + 本地 listener，绕开 DNS），共享 client 不参与。
func TestDoJSONViaAccountTransport(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"uid":"u1","nickname":"routed"}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var directDials, routedDials int64
	c := New()
	// 共享 transport 记拨号（不应被走到）。
	c.HTTP.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt64(&directDials, 1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	routed := newTransportCounting(t, ln, &routedDials)
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return routed, "node-a" })
	// 假域名：只有注入 transport（无视 addr 拨本地）能到达。
	c.WebBaseCN = "http://routed-by-proxy.invalid"

	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	nick, err := c.FetchAccountProfile(a)
	if err != nil {
		t.Fatalf("FetchAccountProfile: %v", err)
	}
	if nick != "routed" {
		t.Fatalf("nick=%q", nick)
	}
	if atomic.LoadInt64(&routedDials) == 0 {
		t.Fatal("流量未走注入 transport")
	}
	if atomic.LoadInt64(&directDials) != 0 {
		t.Fatal("流量走了共享 client（DNS 会失败的路由）")
	}
}

// TestChatStreamContextViaAccountTransport chat SSE 流走注入 transport。
func TestChatStreamContextViaAccountTransport(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var routedDials int64
	c := New()
	c.ChatBaseCN = "http://routed-by-proxy.invalid"
	// 拨号器跑在 transport 自己的 goroutine 里：FailNow/Fatal 只能由测试 goroutine
	// 调用（否则测试直接 abort 且丢失上下文），这里只能 Errorf + 返回错误让请求失败，
	// 并用 atomic 计数供主 goroutine 做终态断言。
	var usedShared int64
	c.ChatHTTP.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt64(&usedShared, 1)
		t.Errorf("chat 流量走了共享 ChatHTTP 拨号器（addr=%s）", addr)
		return nil, net.ErrClosed
	}
	routed := newTransportCounting(t, ln, &routedDials)
	c.SetAccountTransport(func(a *auth.Auth) (*http.Transport, string) { return routed, "node-a" })

	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	rc, status, _, err := c.ChatStreamContext(nil, a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{CacheKeyID: "s"}, nil)
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("empty stream")
	}
	if n := atomic.LoadInt64(&usedShared); n != 0 {
		t.Fatalf("chat 流量走了共享 ChatHTTP（拨号 %d 次）", n)
	}
	if atomic.LoadInt64(&routedDials) == 0 {
		t.Fatal("chat 流量未走注入 transport")
	}
}

// TestClientForCacheKeyedByNodeID S10 回归：client 缓存键是**稳定节点 ID**，不是
// transport 指针。节点内容变化时 proxypool 换新 transport 实例，条目必须原地替换
// （修复前按指针缓存，每换一次版本就永久多留一份 client，永不清除）。
func TestClientForCacheKeyedByNodeID(t *testing.T) {
	c := New()
	tr1 := &http.Transport{}
	tr2 := &http.Transport{}
	a := &auth.Auth{UID: "u1"}
	c.SetAccountTransport(func(x *auth.Auth) (*http.Transport, string) { return tr1, "node-a" })
	r1, ch1 := c.clientFor(a)

	// 同一节点 ID、新 transport 实例（内容更新）→ 条目原地替换。
	c.SetAccountTransport(func(x *auth.Auth) (*http.Transport, string) { return tr2, "node-a" })
	r2, ch2 := c.clientFor(a)
	if r2 == r1 || ch2 == ch1 {
		t.Fatal("transport 换实例后应换 client")
	}
	if r2.Transport != tr2 || ch2.Transport != tr2 {
		t.Fatal("新 client 应使用新 transport")
	}
	if n := len(c.acctClients); n != 1 {
		t.Fatalf("同一节点 ID 应只有 1 个条目，得到 %d（缓存随版本数增长 = 泄漏）", n)
	}
}

// TestEvictAccountClients S10 回归：解绑/失效钩子能真正淘汰条目。
func TestEvictAccountClients(t *testing.T) {
	c := New()
	tr := &http.Transport{}
	c.SetAccountTransport(func(x *auth.Auth) (*http.Transport, string) { return tr, "node-a" })
	a := &auth.Auth{UID: "u1"}
	r1, _ := c.clientFor(a)
	if n := len(c.acctClients); n != 1 {
		t.Fatalf("entries=%d", n)
	}
	if got := c.EvictAccountClients("node-b"); got != 0 {
		t.Fatalf("未知键不应淘汰: %d", got)
	}
	if got := c.EvictAccountClients("node-a"); got != 1 {
		t.Fatalf("应淘汰 1 条，得到 %d", got)
	}
	if n := len(c.acctClients); n != 0 {
		t.Fatalf("淘汰后 entries=%d", n)
	}
	r2, _ := c.clientFor(a)
	if r2 == r1 {
		t.Fatal("淘汰后应重建 client")
	}
}
