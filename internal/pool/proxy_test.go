package pool

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestSetAccountProxy 绑定/解绑/幂等/未知账号。
func TestSetAccountProxy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.AccountProxy("u1"); got != "" {
		t.Fatalf("fresh account proxy=%q want empty", got)
	}
	if !p.SetAccountProxy("u1", "nodeA") {
		t.Fatal("bind should change")
	}
	if got := p.AccountProxy("u1"); got != "nodeA" {
		t.Fatalf("proxy=%q", got)
	}
	// 幂等重绑：false（无变更）。
	if p.SetAccountProxy("u1", "nodeA") {
		t.Fatal("rebind same should be no-op")
	}
	// 改绑。
	if !p.SetAccountProxy("u1", "nodeB") {
		t.Fatal("rebind should change")
	}
	// 解绑（空 id）。
	if !p.SetAccountProxy("u1", "") {
		t.Fatal("unbind should change")
	}
	if got := p.AccountProxy("u1"); got != "" {
		t.Fatalf("after unbind proxy=%q", got)
	}
	// 未知账号。
	if p.SetAccountProxy("ghost", "nodeA") {
		t.Fatal("unknown uid should fail")
	}
	if got := p.AccountProxy("ghost"); got != "" {
		t.Fatalf("unknown uid proxy=%q", got)
	}
}

// TestClearProxyIfMatch 批量解绑：只清命中节点、返回升序 uid、其余绑定不动。
func TestClearProxyIfMatch(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetAccountProxy("u1", "nodeA")
	p.SetAccountProxy("u2", "nodeA")
	p.SetAccountProxy("u3", "nodeB")

	unbound := p.ClearProxyIfMatch(map[string]bool{"nodeA": true})
	if len(unbound) != 2 || !sort.StringsAreSorted(unbound) {
		t.Fatalf("unbound=%v", unbound)
	}
	if unbound[0] != "u1" || unbound[1] != "u2" {
		t.Fatalf("unbound=%v want [u1 u2]", unbound)
	}
	if got := p.AccountProxy("u3"); got != "nodeB" {
		t.Fatalf("u3 绑定被误清: %q", got)
	}
	// 空入参 no-op；重复调用 no-op。
	if got := p.ClearProxyIfMatch(nil); got != nil {
		t.Fatalf("nil ids → %v", got)
	}
	if got := p.ClearProxyIfMatch(map[string]bool{"nodeA": true}); got != nil {
		t.Fatalf("second clear → %v", got)
	}
}

// TestProxyPersistsInState 绑定关系落 state.json（重启不丢、老文件零值兼容）。
func TestProxyPersistsInState(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetAccountProxy("u1", "nodeA")
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	if got := p2.AccountProxy("u1"); got != "nodeA" {
		t.Fatalf("after reload proxy=%q", got)
	}
	// Status 透出。
	st, ok := p2.Status("u1")
	if !ok || st.ProxyID != "nodeA" {
		t.Fatalf("status proxy=%q ok=%v", st.ProxyID, ok)
	}
}
