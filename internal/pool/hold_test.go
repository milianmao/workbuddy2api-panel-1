package pool

import (
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestHoldPausesNewAccount 「新增待确认」：新号落池后不进选号候选，但仍在池中
// （保号任务照常跑），且状态里 hold 与 paused 同时为真。
func TestHoldPausesNewAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-new"})
	if p.Pick() == nil {
		t.Fatal("baseline: a fresh account should be pickable before Hold")
	}
	if !p.Hold("u-new") {
		t.Fatal("Hold should return true for existing uid")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("held account must not be picked, got %+v", got)
	}
	st, ok := p.Status("u-new")
	if !ok || !st.Paused || !st.Hold {
		t.Fatalf("status should be paused+hold, got %+v ok=%v", st, ok)
	}
	if p.Hold("missing") {
		t.Fatal("Hold on missing uid should return false")
	}
}

// TestHoldSurvivesRevive 关键回归：解冻（Revive）是「全清」语义，但不得清掉
// 「新增待确认」的暂停——否则一次解冻就把来路未验证的新号推进线上（实测有 403 11140）。
func TestHoldSurvivesRevive(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-new"})
	p.Hold("u-new")
	p.Revive("u-new")
	if got := p.Pick(); got != nil {
		t.Fatalf("held account revived into rotation, got %+v", got)
	}
	st, _ := p.Status("u-new")
	if !st.Paused || !st.Hold {
		t.Fatalf("Revive must keep paused+hold, got %+v", st)
	}
}

// TestHoldSurvivesBalanceRefresh 签到/余额刷新（ReenableIfCredits）不得解锁待确认账号。
func TestHoldSurvivesBalanceRefresh(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-new"})
	p.Hold("u-new")
	p.ReenableIfCredits("u-new", 2500, 2600)
	if got := p.Pick(); got != nil {
		t.Fatalf("balance refresh must not unhide held account, got %+v", got)
	}
}

// TestResumeClearsHold 人工「恢复选号」= 确认可用：hold 与 paused 一并清除。
func TestResumeClearsHold(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u-new"})
	p.Hold("u-new")
	p.Resume("u-new")
	st, _ := p.Status("u-new")
	if st.Paused || st.Hold {
		t.Fatalf("Resume should clear paused+hold, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u-new" {
		t.Fatalf("resumed account should be pickable, got %+v", got)
	}
}

// TestHoldPersistsAcrossReload 待确认标记必须落盘：重启/重建容器后仍须人工确认，
// 否则一次重启就把未验证的新号放上流量。
func TestHoldPersistsAcrossReload(t *testing.T) {
	withNoPickGap(t)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u-new"})
	p.Hold("u-new")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u-new"})
	st, ok := p2.Status("u-new")
	if !ok || !st.Paused || !st.Hold {
		t.Fatalf("hold lost after reload: %+v ok=%v", st, ok)
	}
	if got := p2.Pick(); got != nil {
		t.Fatalf("held account picked after reload, got %+v", got)
	}
}
