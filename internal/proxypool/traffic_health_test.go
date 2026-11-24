package proxypool

import "testing"

// TestSelectLockedAllowsPendingButSkipsDead 选路判据：只排除**被判死**的节点。
//
// 这条判据原来是 == "healthy"，两个后果都在现场复现过：
//   - 节点被探针判死的瞬间，钉在它上面的号 100% 报 no healthy proxy available，
//     而且 do=0ms（请求根本没拨号），上层还不换号，于是连着刷 503；
//   - 进程刚起来时所有节点都是 pending，要等 success_threshold 轮探测才转 healthy，
//     每次重启都有约两分钟的「所有 fixed 号一律拒单」假死窗口。
func TestSelectLockedAllowsPendingButSkipsDead(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://127.0.0.1:1/")
	if err := m.Bind("u", Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	pick := func() (node, bool, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.selectLocked("u", "cn")
	}
	// pending（没测过）：放行——没证据不等于判死
	if n, ok, err := pick(); err != nil || !ok || n.ID != id {
		t.Fatalf("pending 节点应放行: %+v ok=%v err=%v", n, ok, err)
	}
	// 判死：仍然拒绝（快速失败，交给上层换号；否则每个请求白等一次拨号超时）
	m.mu.Lock()
	m.health[id] = health{status: "unhealthy"}
	m.mu.Unlock()
	if _, ok, err := pick(); ok || !IsError(err) {
		t.Fatalf("判死节点应拒绝: ok=%v err=%v", ok, err)
	}
	// 真实流量打通一次即恢复，不等两轮探测
	m.noteTraffic(id, true, "")
	if n, ok, err := pick(); err != nil || !ok || n.ID != id {
		t.Fatalf("真实流量成功后应立刻可用: %+v ok=%v err=%v", n, ok, err)
	}
}

// TestTrafficFailureDemotesNode 真实业务流量反馈节点健康（noteTraffic）。
//
// 探测每 60s 一轮、连败 failure_threshold 次才判死，一个「真的打不通」的节点
// 最多能白吃三分钟的业务流量；节点恢复也要再等两轮探测。真实流量是最便宜的探针。
func TestTrafficFailureDemotesNode(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://127.0.0.1:1/")
	snap := func() health {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.health[id]
	}
	// 打通一次 → healthy，且连续成功数 +1
	m.noteTraffic(id, true, "")
	if h := snap(); h.status != "healthy" || h.successes != 1 {
		t.Fatalf("成功反馈: %+v", h)
	}
	// 单次失败不判死（可能是偶发 RST），但 reason 要留痕
	m.noteTraffic(id, false, "proxy request failed")
	if h := snap(); h.status == "unhealthy" {
		t.Fatalf("单次失败不该判死: %+v", h)
	} else if h.lastError != "proxy request failed" {
		t.Fatalf("失败原因未留痕: %+v", h)
	}
	// 连续第二次 → 判死，选路立刻避开
	m.noteTraffic(id, false, "proxy request timed out")
	if h := snap(); h.status != "unhealthy" {
		t.Fatalf("连续失败应判死: %+v", h)
	}
	if got := m.Snapshot().Proxies[0].Status; got != "unhealthy" {
		t.Fatalf("快照状态 = %s", got)
	}
	// 再打通一次 → 立刻恢复，并清掉上次错误
	m.noteTraffic(id, true, "")
	if h := snap(); h.status != "healthy" || h.failures != 0 || h.lastError != "" {
		t.Fatalf("恢复后状态: %+v", h)
	}
	// 空 ID 一律忽略：不该悄悄记到某个共享槽里
	m.noteTraffic("", false, "x")
}
