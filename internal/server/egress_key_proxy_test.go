package server

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestEgressKeyForUsesProxyNode 出口级退避桶必须按**真实出口**分桶。
//
// 之前一律返回 "direct:<realm>"，把所有经代理的失败都记进本域共用的直连桶里。
// 一个节点坏掉就能把这个桶顶到 give-up 阈值，之后同域每个请求只要第一个被选中的
// 号绑在那个节点上就立刻终止轮转——现场表现是 try=1、do=0ms 的瞬时 503 连刷几十条。
func TestEgressKeyForUsesProxyNode(t *testing.T) {
	m, err := proxypool.New(filepath.Join(t.TempDir(), "proxy-pool.json"),
		http.DefaultTransport.(*http.Transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	res, err := m.Import("http://127.0.0.1:1/")
	if err != nil || len(res) != 1 || res[0].Status != "added" {
		t.Fatalf("import: %+v %v", res, err)
	}
	node := res[0].ID
	if err := m.Bind("pinned", proxypool.Binding{Mode: "fixed", ProxyID: node}); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("direct", proxypool.Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}

	h := &Handler{cfg: Config{Upstream: &upstream.Client{ProxyPool: m}}}
	if got := h.egressKeyFor(&auth.Auth{UID: "pinned"}); got != "proxy:"+node {
		t.Fatalf("钉住的号应单独成桶: %q", got)
	}
	// 显式直连、未绑定、以及没有 h.cfg.ProxyPool 的情形都退回直连口径
	if got := h.egressKeyFor(&auth.Auth{UID: "direct"}); got != "direct:cn" {
		t.Fatalf("显式直连: %q", got)
	}
	if got := h.egressKeyFor(&auth.Auth{UID: "unbound"}); got != "direct:cn" {
		t.Fatalf("未绑定: %q", got)
	}
	empty := &Handler{}
	if got := empty.egressKeyFor(&auth.Auth{UID: "x"}); got != "direct:cn" {
		t.Fatalf("无代理池: %q", got)
	}
	if got := empty.egressKeyFor(nil); got != "direct:cn" {
		t.Fatalf("nil 账号: %q", got)
	}
}
