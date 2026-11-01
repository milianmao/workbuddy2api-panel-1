package main

import (
	"net/http"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestProxyExitKeyOf S6 回归（#122 引擎版）：WAF 出口分组只认「固定绑定」。
//
// 与旧池的语义差异（有意为之，跟随上游）：绑定到停用/不可用节点时**不回落直连**，
// 而是让请求直接报错（宁可不发，也不悄悄从宿主 IP 出去）。因此 ExitKey 仍返回
// 节点 ID——它代表「这个号被钉在哪个出口」，不代表该出口当前可用。
func TestProxyExitKeyOf(t *testing.T) {
	pp, err := proxypool.New("", &http.Transport{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pp.Close)
	// 注意：parseURL 拒绝带 fragment 的代理 URL（#node-a 会被判 invalid），
	// 所以这里用纯 URL，节点名由 Name（host:port）承载。
	res, err := pp.Import("http://u:p@127.0.0.1:1\nhttp://u:p@127.0.0.1:2")
	if err != nil || len(res) != 2 {
		t.Fatalf("Import=%+v err=%v", res, err)
	}
	for _, one := range res {
		if one.Status != "added" || one.ID == "" {
			t.Fatalf("导入应全部 added 且有 id，得到 %+v", res)
		}
	}
	nodeA, nodeB := res[0].ID, res[1].ID

	exitOf := proxyExitKeyOf(pp)

	if err := pp.Bind("u1", proxypool.Binding{Mode: "fixed", ProxyID: nodeA}); err != nil {
		t.Fatalf("绑定已存在节点应成功: %v", err)
	}
	if err := pp.Bind("u2", proxypool.Binding{Mode: "fixed", ProxyID: "ghost"}); err == nil {
		t.Fatal("绑定不存在的节点必须报错（旧行为是静默回落直连）")
	}
	if err := pp.Bind("u3", proxypool.Binding{Mode: "direct"}); err != nil {
		t.Fatalf("显式直连应成功: %v", err)
	}

	if got := exitOf("u1"); got != nodeA {
		t.Fatalf("固定绑定应返回节点 id，得到 %q", got)
	}
	if got := exitOf("u2"); got != "" {
		t.Fatalf("绑定失败后应仍在随机组，得到 %q", got)
	}
	if got := exitOf("u3"); got != "" {
		t.Fatalf("direct 应归直连组，得到 %q", got)
	}
	if got := exitOf("no-such-uid"); got != "" {
		t.Fatalf("未知账号应归直连组，得到 %q", got)
	}

	// 停用节点：分组不变（仍是该出口），但上游 selectLocked 会拒绝下发。
	off := false
	if err := pp.Update(nodeA, proxypool.Input{Enabled: &off}); err != nil {
		t.Fatalf("停用节点失败: %v", err)
	}
	if got := exitOf("u1"); got != nodeA {
		t.Fatalf("停用节点后出口分组不应变化，得到 %q", got)
	}
	b := pp.Snapshot().Bindings["u1"]
	if b.Mode != "fixed" || b.ProxyID != nodeA {
		t.Fatalf("停用节点不应改写绑定，得到 %+v", b)
	}

	// 重新启用 → 绑定照旧（不需要重绑）。
	on := true
	if err := pp.Update(nodeA, proxypool.Input{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if got := exitOf("u1"); got != nodeA {
		t.Fatalf("重新启用后应仍为独立出口组，得到 %q", got)
	}
	// 解绑 = 回到随机池（不写 bindings 条目）。
	if err := pp.Unbind("u1"); err != nil {
		t.Fatal(err)
	}
	if got := exitOf("u1"); got != "" {
		t.Fatalf("解绑后应归随机/直连组，得到 %q", got)
	}
	_ = nodeB
}

// TestProxyExitKeyOfNilSafe 装配缺失（nil）时不得 panic（返回直连组）。
func TestProxyExitKeyOfNilSafe(t *testing.T) {
	exitOf := proxyExitKeyOf(nil)
	if got := exitOf("u1"); got != "" {
		t.Fatalf("nil 装配应归直连组，得到 %q", got)
	}
}

// TestProxypoolDialTimeoutMatchesUpstream S1 口径：注入给代理池的建连上限就是
// upstream 共享 dialer 的上限（同一常量，不许两处硬编码漂移）。
func TestProxypoolDialTimeoutMatchesUpstream(t *testing.T) {
	if got := upstream.NewDNSDialer(nil).Timeout; got != upstreamDialTimeout() {
		t.Fatalf("代理池 dialer 上限 %v 与 upstream %v 不一致", got, upstreamDialTimeout())
	}
}
