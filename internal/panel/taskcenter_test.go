package panel

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestRunGrowthQueueNowUnguarded 排程入口在未接线（无上游）时安全返回，不 panic。
func TestRunGrowthQueueNowUnguarded(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	total, msg := p.RunGrowthQueueNow()
	if total != 0 || msg == "" {
		t.Errorf("total=%d msg=%q want 0 + 原因文案", total, msg)
	}
}

// TestRunGrowthQueueNowBusy 账号被其它任务动作占用（per-account 锁）时，排程轮次
// 不重复启动该账号的后台作业——与手动/队列入口共用同一把锁。
func TestRunGrowthQueueNowBusy(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	p.cfg.AutoTasksEnabled = func() bool { return true }
	p.cfg.Pool.Add(&auth.Auth{UID: "busy-1", AccessToken: "t"})
	if err := p.StartTaskJobs(context.Background(), filepath.Join(t.TempDir(), "task-jobs.json")); err != nil {
		t.Fatal(err)
	}
	defer p.StopTaskJobs()
	if !p.tryLockAccount("busy-1") {
		t.Fatal("占锁失败")
	}
	defer p.unlockAccount("busy-1")
	if total, _ := p.RunGrowthQueueNow(); total != 0 {
		t.Errorf("锁被占用时不应启动后台作业：total=%d", total)
	}
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Errorf("忙碌账号不应产生作业：%+v", jobs)
	}
}

// TestRunQueueItemsSummaryAndNotify 一轮跑完的汇总文案与推送开关：
// 排程轮次（notify=true）推一条，手动轮次（false）只落日志。
func TestRunQueueItemsSummaryAndNotify(t *testing.T) {
	p := New(Config{Version: "test"})
	var got string
	p.SetNotifier(func(s string) { got = s })
	q := p.queue()
	q.mu.Lock()
	q.seq = 3
	q.running = true
	q.items = []queueItem{
		{UID: "u1", Code: "Model_chat", Status: "done", Credit: 100, Energy: 5},
		{UID: "u2", Code: "black_cat", Status: "skipped"},
		{UID: "u2", Code: "Expert_5", Status: "error"},
	}
	items := q.items
	q.mu.Unlock()

	p.runQueueItems(nil, items, 1, true) // 空账号组：只走收尾（汇总 + 推送）
	for _, want := range []string{"第 3 轮 3 项", "完成 1 / 跳过 1 / 失败 1", "领奖 100 分 +5 能", "失败项 Expert_5"} {
		if !strings.Contains(got, want) {
			t.Errorf("汇总 %q 缺少 %q", got, want)
		}
	}
	if q.isRunning() {
		t.Error("轮次结束后 running 应为 false")
	}

	got = ""
	q.mu.Lock()
	q.running = true
	q.mu.Unlock()
	p.runQueueItems(nil, items, 1, false)
	if got != "" {
		t.Errorf("手动轮次不应推送：%q", got)
	}
}

// TestBuildQueueItemsPerAccountCap 每号限 N：队列条目按账号只保留前 N 条，
// 未排入的条数如实统计（日志要说清「还剩多少没跑」）。
// 面板「每号做 1 个」按钮靠 perAccount=1 把每次动作压到每号一条。
func TestBuildQueueItemsPerAccountCap(t *testing.T) {
	accts := []queueAccount{
		{a: &auth.Auth{UID: "u1", Nickname: "甲"}, grow: []upstream.Task{
			{TaskCode: "chat_5"}, {TaskCode: "first_buddy"}, {TaskCode: "skill_1"},
		}},
		{a: &auth.Auth{UID: "u2", Nickname: "乙"}, grow: []upstream.Task{{TaskCode: "chat_5"}}},
	}
	items, trimmed := buildQueueItems(accts, 1)
	if len(items) != 2 {
		t.Fatalf("每号限 1 应只排 2 条，实得 %d：%+v", len(items), items)
	}
	if trimmed != 2 {
		t.Errorf("未排入条数应为 2，实得 %d", trimmed)
	}
	if items[0].UID != "u1" || items[0].Code != "chat_5" || items[0].Kind != "growth" || items[0].Status != "pending" {
		t.Errorf("首条应取 u1 的第一个候选，实得 %+v", items[0])
	}
	if items[1].UID != "u2" || items[1].Code != "chat_5" {
		t.Errorf("第二条应是 u2 的唯一候选，实得 %+v", items[1])
	}

	// 缺省 0 = 不限：旧行为必须逐字不变（全部待办都排进去，不统计截断）。
	all, trimmedAll := buildQueueItems(accts, 0)
	if len(all) != 4 || trimmedAll != 0 {
		t.Errorf("不限量时应排 4 条且不截断，实得 %d 条 / 截断 %d", len(all), trimmedAll)
	}
}

// TestSortGrowthPicksClaimFirstAndStuckDeferred 每号限 1 的候选顺序：
// 已达标的先拿（零消耗），上轮打空的让位，其余沿用 autoActions 依赖序。
func TestSortGrowthPicksClaimFirstAndStuckDeferred(t *testing.T) {
	build := func() []upstream.Task {
		return []upstream.Task{
			{TaskCode: "skill_1"},
			{TaskCode: "chat_5"},
			{TaskCode: "Hp_Appearance", Target: 1, Current: 1, ProgressKnown: true}, // 已达标未领
			{TaskCode: "RichMeow_Chat"},
			{TaskCode: "black_cat"}, // 无自动动作、未达标：既跑不了也领不到
		}
	}
	codes := func(tasks []upstream.Task) string {
		out := make([]string, len(tasks))
		for i, task := range tasks {
			out[i] = task.TaskCode
		}
		return strings.Join(out, ",")
	}

	// 无上轮：达标项先拿，其余按 autoActions 序号（chat_5=0 < RichMeow=2 < skill_1=11）。
	grow := build()
	sortGrowthPicks(grow, nil)
	if got, want := codes(grow), "Hp_Appearance,chat_5,RichMeow_Chat,skill_1,black_cat"; got != want {
		t.Errorf("无上轮顺序 %s，期望 %s", got, want)
	}

	// chat_5 上轮跑了但没入账：让位给没试过的，避免它吃掉每号唯一的名额。
	grow = build()
	sortGrowthPicks(grow, map[string]string{"chat_5": "awaiting_progress"})
	if got, want := codes(grow), "Hp_Appearance,RichMeow_Chat,skill_1,chat_5,black_cat"; got != want {
		t.Errorf("打空让位顺序 %s，期望 %s", got, want)
	}

	// 上轮 done / 没跑过的都不降级（done 通常已不在待办里；在的话仍按依赖序走）。
	grow = build()
	sortGrowthPicks(grow, map[string]string{"chat_5": "done"})
	if got, want := codes(grow), "Hp_Appearance,chat_5,RichMeow_Chat,skill_1,black_cat"; got != want {
		t.Errorf("已完成项顺序 %s，期望 %s", got, want)
	}
}

// TestPrevStatusByAccount 上一轮结果索引只收 growth 条目，供每号限量的让位判断。
func TestPrevStatusByAccount(t *testing.T) {
	p := New(Config{Version: "test"})
	q := p.queue()
	if prev := q.prevStatusByAccount(); prev != nil {
		t.Errorf("首轮（无 items）应返回 nil，实得 %+v", prev)
	}
	q.mu.Lock()
	q.items = []queueItem{
		{UID: "u1", Kind: "growth", Code: "chat_5", Status: "awaiting_progress"},
		{UID: "u1", Kind: "growth", Code: "skill_1", Status: "done"},
		{UID: "u2", Kind: "account", Code: "", Status: "skipped"},
	}
	q.mu.Unlock()
	prev := q.prevStatusByAccount()
	if got := prev["u1"]["chat_5"]; got != "awaiting_progress" {
		t.Errorf("u1/chat_5 状态 %q，期望 awaiting_progress", got)
	}
	if got := prev["u1"]["skill_1"]; got != "done" {
		t.Errorf("u1/skill_1 状态 %q，期望 done", got)
	}
	if _, ok := prev["u2"]; ok {
		t.Error("非 growth 条目不应进索引（账号级跳过不是任务结果）")
	}

	if perAccountNote(0, 9) != "" {
		t.Error("不限量时日志后缀应为空")
	}
	if note := perAccountNote(1, 3); !strings.Contains(note, "每号最多 1 个") || !strings.Contains(note, "3") {
		t.Errorf("限量日志后缀 %q 缺少上限/未排入条数", note)
	}
}
