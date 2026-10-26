package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// restoreWafWindow 还原短窗注入（与 wafIPWindow 的测试注入先例一致）。
func restoreWafWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := wafIPWindow
	wafIPWindow = d
	t.Cleanup(func() { wafIPWindow = old })
}

func newWafTestHandler(t *testing.T, exitOf func(uid string) string) *Handler {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at"})
	return NewHandler(Config{Pool: p, Upstream: nil, ExitTagOf: exitOf})
}

// TestWafGateGroupsByExit 同一出口的两个 UID 达阈值激活；另一出口不受影响；
// 直连组（""）独立计数。这正是「同出口连坐、异出口互不连坐」的分组语义。
func TestWafGateGroupsByExit(t *testing.T) {
	restoreWafWindow(t, 2*time.Second)
	exits := map[string]string{"u1": "nodeA", "u2": "nodeA", "u3": "nodeB"}
	h := newWafTestHandler(t, func(uid string) string { return exits[uid] })

	// u1（nodeA）单号命中：不激活。
	if h.wafGateForUID("u1").noteWaf("u1") {
		t.Fatal("单号命中不应激活")
	}
	// u3（nodeB）单号命中：不激活，且不影响 nodeA 的计数。
	if h.wafGateForUID("u3").noteWaf("u3") {
		t.Fatal("nodeB 单号命中不应激活")
	}
	// u2（nodeA）命中：nodeA 两个不同 UID 达阈值 → 激活。
	if !h.wafGateForUID("u2").noteWaf("u2") {
		t.Fatal("nodeA 两个 UID 应激活")
	}
	// 激活只作用于 nodeA：nodeB 与直连组未激活。
	if !h.wafGateForExit("nodeA").active() {
		t.Fatal("nodeA 应处于激活态")
	}
	if h.wafGateForExit("nodeB").active() {
		t.Fatal("nodeB 不应被 nodeA 连坐")
	}
	if h.wafGateForExit("").active() {
		t.Fatal("直连组不应被代理组连坐")
	}
	// WAFActive 全局观测：任一组激活即 true。
	if !h.WAFActive() {
		t.Fatal("WAFActive 应报告激活")
	}

	// 窗口过后自然解除。
	deadline := time.Now().Add(3 * time.Second)
	for h.wafGateForExit("nodeA").active() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.wafGateForExit("nodeA").active() {
		t.Fatal("nodeA 窗口过后应解除")
	}
	if h.WAFActive() {
		t.Fatal("全部解除后 WAFActive 应为 false")
	}
}

// TestWafGateDirectGroup 直连组（""）：两个未绑定代理的 UID 命中即激活。
func TestWafGateDirectGroup(t *testing.T) {
	restoreWafWindow(t, 2*time.Second)
	h := newWafTestHandler(t, nil) // 未注入 ExitTagOf → 全部直连组（旧行为）
	if h.wafGateForUID("u1").noteWaf("u1") {
		t.Fatal("单号命中不应激活")
	}
	if !h.wafGateForUID("u2").noteWaf("u2") {
		t.Fatal("直连组两个 UID 应激活")
	}
	if !h.wafGateForExit("").active() {
		t.Fatal("直连组应处于激活态")
	}
}

// TestWafGateSharedProxy 连坐语义：共用同一代理的账号在同一组内（同 IP 语义）。
func TestWafGateSharedProxy(t *testing.T) {
	restoreWafWindow(t, 2*time.Second)
	h := newWafTestHandler(t, func(string) string { return "nodeA" })
	if h.wafGateForUID("u1").noteWaf("u1") {
		t.Fatal("单号命中不应激活")
	}
	// u2 与 u1 是同一个 exitTag → 同一个 gate 实例 → 连坐激活。
	if !h.wafGateForUID("u2").noteWaf("u2") {
		t.Fatal("共用代理的两个 UID 应在同一组激活")
	}
	if h.wafGateForUID("u1") != h.wafGateForUID("u2") {
		t.Fatal("同出口应共享同一 gate 实例")
	}
}

// TestWafGateOfUIDResolvesOnce S10 回归：轮转循环同时需要 exitKey 与 gate，
// 必须一次解析（ExitTagOf 可能要查节点表/持锁），不得重复调用闭包。
func TestWafGateOfUIDResolvesOnce(t *testing.T) {
	restoreWafWindow(t, 2*time.Second)
	var calls int
	h := newWafTestHandler(t, func(uid string) string {
		calls++
		return "nodeA"
	})
	exit, gate := h.wafGateOfUID("u1")
	if exit != "nodeA" {
		t.Fatalf("exit=%q", exit)
	}
	if gate != h.wafGateForExit("nodeA") {
		t.Fatal("gate 应属于该出口")
	}
	if calls != 1 {
		t.Fatalf("ExitTagOf 应只解析一次，实际调用 %d 次", calls)
	}
	// 未注入 ExitTagOf → 恒直连组，且不 panic。
	h2 := newWafTestHandler(t, nil)
	if exit, gate := h2.wafGateOfUID("u1"); exit != "" || gate != h2.wafGateForExit("") {
		t.Fatalf("未注入时应归直连组: exit=%q", exit)
	}
}

// TestChatWaf403ResolvesExitOnce S10 回归：轮转里命中的 WAF 403 只解析一次出口。
// 修复前 lastWafExit = exitTagOfUID(...) 与 wafGateForUID(...) 各调一次闭包
// （实际 ExitTagOf 要查节点表/持锁），同一次判断白跑两遍。
func TestChatWaf403ResolvesExitOnce(t *testing.T) {
	restoreWafWindow(t, 2*time.Second)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "", false // 空体 403 → ErrWafBlock
	})
	p := testPoolWith(
		&auth.Auth{UID: "w1", AccessToken: "at-w1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "w2", AccessToken: "at-w2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		ExitTagOf: func(uid string) string { calls++; return "nodeA" },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("WAF 403 不应成功: %d", rec.Code)
	}
	// 两个号各命中一次 WAF 403（阈值 2 激活），出口解析必须恰好 2 次 = 一次/命中。
	if calls != 2 {
		t.Fatalf("ExitTagOf 调用 %d 次，want 2（每次 WAF 403 只解析一次出口）", calls)
	}
	if !h.WAFActive() {
		t.Fatal("两个号同出口命中应激活 IP 级拦截")
	}
}

func TestWafIPGateStateModel(t *testing.T) {
	old := wafIPWindow
	wafIPWindow = 60 * time.Millisecond
	defer func() { wafIPWindow = old }()

	g := &wafIPGate{}
	for i := 0; i < 5; i++ {
		if g.noteWaf("u1") {
			t.Fatal("单号反复 403 永不应触发 IP 级拦截")
		}
	}
	if g.active() {
		t.Fatal("单号命中不应激活")
	}
	// 窗内第二个不同号 → 激活。
	if g.noteWaf("u1") {
		// u1 已在窗内，本次仍只有 1 个不同号
	}
	if !g.noteWaf("u2") {
		t.Fatal("窗内第二个不同号命中应激活")
	}
	if !g.active() {
		t.Fatal("激活期内 active() 应为 true")
	}
	// 激活期内新命中：仍返回 true（fail-fast 语义）。
	if !g.noteWaf("u3") {
		t.Fatal("激活期内新命中应继续 fail-fast")
	}
	// 窗口到期自然解除。
	time.Sleep(80 * time.Millisecond)
	if g.active() {
		t.Fatal("窗口到期应自然解除")
	}
	// 解除后旧账已清：单号命中不激活，需再集满两个不同号。
	if g.noteWaf("u4") {
		t.Fatal("解除后单号命中不应立即激活（旧账应已清）")
	}
	if !g.noteWaf("u5") {
		t.Fatal("解除后重新集满两个不同号应再次激活")
	}
}
