package main

import (
	"net/http"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// TestProxyExitKeyOf S6 回归：WAF 出口分组必须按**实际出口**（TransportFor 的
// 判定口径），而不是绑定意图。三种回落（未绑定 / 节点不存在 / 节点停用）全部
// 归直连组 ""，只有「绑定且启用」才返回节点 ID。
func TestProxyExitKeyOf(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at"})

	// 注入底座，才能验证「ExitKey 非空 ⇔ TransportFor 非 nil」的一致性。
	pp := proxypool.New("", func() *http.Transport { return &http.Transport{} })
	t.Cleanup(pp.Close)
	res, err := pp.AddRaw("http://u:p@127.0.0.1:1#node-a\nhttp://u:p@127.0.0.1:2#node-b")
	if err != nil || res.Imported != 2 {
		t.Fatalf("AddRaw=%+v err=%v", res, err)
	}
	ids := map[string]string{}
	for _, n := range pp.List() {
		ids[n.Tag] = n.ID
	}
	nodeA := ids["node-a"]
	nodeB := ids["node-b"]

	exitOf := proxyExitKeyOf(p, pp)

	p.SetAccountProxy("u1", nodeA)   // 正常
	p.SetAccountProxy("u2", "ghost") // 节点不存在
	p.SetAccountProxy("u3", nodeB)   // 正常，随后停用
	if _, err := pp.Toggle(nodeB); err != nil {
		t.Fatal(err)
	}

	if got := exitOf("u1"); got != nodeA {
		t.Fatalf("正常绑定应返回节点 id，得到 %q", got)
	}
	if got := exitOf("u2"); got != "" {
		t.Fatalf("绑定不存在的节点应归直连组，得到 %q", got)
	}
	if got := exitOf("u3"); got != "" {
		t.Fatalf("绑定停用节点应归直连组，得到 %q", got)
	}
	if got := exitOf("no-such-uid"); got != "" {
		t.Fatalf("未知账号应归直连组，得到 %q", got)
	}

	// 与 TransportFor 口径一致：ExitKey 非空 ⇔ TransportFor 非 nil。
	if pp.TransportFor(nodeA) == nil {
		t.Fatal("启用的已绑定节点应有 transport")
	}
	if pp.TransportFor(nodeB) != nil {
		t.Fatal("停用节点不该有 transport（否则分组与出口不一致）")
	}
	// 重新启用 → 回到独立分组。
	if _, err := pp.Toggle(nodeB); err != nil {
		t.Fatal(err)
	}
	if got := exitOf("u3"); got != nodeB {
		t.Fatalf("重新启用后应回到独立出口组，得到 %q", got)
	}
}

// TestProxyExitKeyOfNilSafe 装配缺失（nil）时不得 panic（返回直连组）。
func TestProxyExitKeyOfNilSafe(t *testing.T) {
	exitOf := proxyExitKeyOf(nil, nil)
	if got := exitOf("u1"); got != "" {
		t.Fatalf("nil 装配应归直连组，得到 %q", got)
	}
}

// TestProxypoolDialTimeoutMatchesUpstream S1 口径：注入给 proxypool 的建连上限就是
// upstream 共享 transport 的 dialTimeout（同一常量，不许两处硬编码漂移）。
func TestProxypoolDialTimeoutMatchesUpstream(t *testing.T) {
	if proxypool.DefaultNodeDialTimeout != upstreamDialTimeout() {
		t.Fatalf("proxypool 默认建连上限 %v 与 upstream %v 不一致",
			proxypool.DefaultNodeDialTimeout, upstreamDialTimeout())
	}
}
