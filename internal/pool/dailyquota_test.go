package pool

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// TestDailyQuotaRoutingAndHotChanges 验证限额是独立路由门禁，不能被绑定、兜底或余额刷新绕过。
func TestDailyQuotaRoutingAndHotChanges(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "limited"})
	p.SetDailyCreditLimits(0.3, nil)
	for i := 0; i < 3; i++ {
		p.RecordDailyCredit("limited", 0.1, true)
	}
	st, _ := p.Status("limited")
	if st.DailyQuota.Used != 0.3 || !st.DailyQuota.Exceeded || st.Disabled || st.Paused {
		t.Fatalf("daily quota status = %+v", st)
	}
	if p.Pick() != nil || p.PickByUID("limited") != nil || p.PickByUIDForModel("limited", "free") != nil ||
		len(p.AvailableUIDs()) != 0 || len(p.AvailableUIDsForModel("free")) != 0 ||
		len(p.AvailableUIDsForRealm("cn")) != 0 || len(p.WeightedAvailableUIDsForModelRealm("free", "cn")) != 0 ||
		len(p.AvailableUIDsForModelRealm("free", "cn")) != 0 || p.ServableNow() || p.Acquire("limited") {
		t.Fatal("日限额账号仍能通过选号、绑定、租约或健康探测接单")
	}
	if model, all := p.CapacityForModel("free", "cn"); model != 0 || all != 0 {
		t.Fatalf("limited capacity = %d/%d", model, all)
	}
	var limited *DailyQuotaError
	if !errors.As(p.CheckDailyQuota("limited"), &limited) || p.DailyQuotaErrorForRealm("cn", "free") == nil {
		t.Fatal("缺少明确的账号日限额错误")
	}
	p.ReenableIfCredits("limited", 100, 100)
	p.Resume("limited")
	if p.Pick() != nil {
		t.Fatal("刷新余额或恢复选号清除了日限额")
	}
	p.Cooldown("limited", CoolSoft, time.Minute, "test")
	if p.Pick() != nil {
		t.Fatal("冷却兜底绕过日限额")
	}
	p.Add(&auth.Auth{UID: "other"})
	if got := p.Pick(); got == nil || got.UID != "other" {
		t.Fatalf("未切换到其他账号：%v", got)
	}
	zero := 0.0
	p.SetDailyCreditLimits(0.3, map[string]*float64{"limited": &zero})
	if err := p.CheckDailyQuota("limited"); err != nil {
		t.Fatalf("显式不限额未生效：%v", err)
	}
	p.SetDailyCreditLimits(0.5, map[string]*float64{"limited": nil})
	st, _ = p.Status("limited")
	if st.DailyQuota.Override != nil || st.DailyQuota.Used != 0.3 || st.DailyQuota.Limit != 0.5 || st.DailyQuota.Exceeded {
		t.Fatalf("继承/热调整清除了用量或未生效：%+v", st.DailyQuota)
	}
}

// TestDailyQuotaPersistenceAndCST 验证达额即落盘、快照恢复、北京时间切日及缺失回执标记。
func TestDailyQuotaPersistenceAndCST(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := New(path)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDailyCreditLimits(1, nil)
	p.RecordDailyCredit("u1", 0, false)
	p.RecordDailyCredit("u1", 1, true)
	restored := New(path) // 不手动 Flush：越过额度必须已经同步保存。
	defer restored.Close()
	restored.Add(&auth.Auth{UID: "u1"})
	restored.SetDailyCreditLimits(1, nil)
	st, _ := restored.Status("u1")
	if !st.DailyQuota.Exceeded || !st.DailyQuota.UsageIncomplete || st.DailyQuota.Used != 1 {
		t.Fatalf("重启丢失限额状态：%+v", st.DailyQuota)
	}
	snapshot := p.stateOverviewLocked()
	fromSnapshot := New("")
	defer fromSnapshot.Close()
	fromSnapshot.applyAccountsLocked(snapshot.Accounts)
	fromSnapshot.SetDailyCreditLimits(1, nil)
	if err := fromSnapshot.CheckDailyQuota("u1"); err == nil {
		t.Fatal("快照恢复丢失日限额")
	}
	before := time.Date(2026, 10, 26, 15, 59, 59, 0, time.UTC)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.dailyCredit.Day = cst.Day(before)
	q := p.dailyQuotaOf(e, before)
	next := p.dailyQuotaOf(e, before.Add(time.Second))
	p.mu.Unlock()
	if !q.Exceeded || q.ResetAt.Hour() != 0 || q.ResetAt.Location() != cst.Loc || next.Used != 0 || next.Exceeded || next.UsageIncomplete {
		t.Fatalf("北京时间切日不正确：before=%+v after=%+v", q, next)
	}
}

// TestDailyQuotaConcurrentObservedCharges 验证不限额时也累计，在途完成的并发扣费不丢失。
func TestDailyQuotaConcurrentObservedCharges(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { p.RecordDailyCredit("u1", 0.1, true) })
	}
	wg.Wait()
	p.SetDailyCreditLimits(1, nil)
	st, _ := p.Status("u1")
	if st.DailyQuota.Used != 2 || !st.DailyQuota.Exceeded {
		t.Fatalf("并发在途扣费丢失：%+v", st.DailyQuota)
	}
}

// TestDailyQuotaModelView 模型锁视图不能把已达日限额的账号计作可服务。
func TestDailyQuotaModelView(t *testing.T) {
	p := lockViewPool(t)
	defer p.Close()
	p.SetDailyCreditLimits(1, nil)
	p.RecordDailyCredit("u2", 1, true)
	setModelCooldown(p, "u1", "m", time.Now().Add(time.Hour), "model cooling", false)
	rows := p.ModelLockView()
	if len(rows) != 1 || rows[0].Servable != 0 || rows[0].State != "starved" {
		t.Fatalf("日限额账号被计作可服务：%+v", rows)
	}
}
