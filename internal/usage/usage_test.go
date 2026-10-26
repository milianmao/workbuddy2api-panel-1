package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	// 缓存命中/未命中随桶累计，并出现在按账号/按模型的聚合行里（面板命中率列的来源）。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{CacheHitTokens: 8960, CacheMissTokens: 197, HasCache: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 4 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 4/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.CacheHitTok != 8960 || s.Totals.CacheMissTok != 197 {
		t.Fatalf("cache = %d/%d, want 8960/197", s.Totals.CacheHitTok, s.Totals.CacheMissTok)
	}
	if s.Totals.Credits != 1.5 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 150 || s.Totals.CreditsPer1MTokens != 10000 {
		t.Fatalf("credit totals = %+v, want credits=1.5 samples=1 tokens=150 ratio=10000", s.Totals)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
	if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "uid1" ||
		s.CreditByAccount[0].CreditSamples != 1 || s.CreditByAccount[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_account = %+v, want one uid1 row", s.CreditByAccount)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "cn:glm-5.2" ||
		s.CreditByModel[0].Rate != "0.05" || s.CreditByModel[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_model = %+v, want one cn:glm-5.2 rate=0.05 row", s.CreditByModel)
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// 版本 1 文件没有积分字段：按零值恢复，旧 Token 数据保持可见且不产生伪比例。
func TestLoadLegacyWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.TotalTokens != 42 || s.Totals.CreditSamples != 0 || s.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", s.Totals)
	}
	if len(s.CreditByAccount) != 0 || len(s.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", s.CreditByAccount, s.CreditByModel)
	}
}

// 小时桶折叠为日桶时必须保留积分、样本数和匹配 Token，比例不能因 Rollup 漂移。
func TestCreditSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	if s.Totals.Credits != 5 || s.Totals.CreditSamples != 2 || s.Totals.CreditTokens != 400 || s.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", s.Totals)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.5" || s.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", s.CreditByModel)
	}
}

// 模型维度按“域:模型 + 生效倍率”合并；同倍率跨账号/时间合并，不同倍率拆行。
func TestCreditDimensionsRateGrouping(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 2 || len(s.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", s.CreditByAccount, s.CreditByModel)
	}
	if s.CreditByModel[0].Key != "cn:glm-5.2" || s.CreditByModel[0].Rate != "0.8" ||
		s.CreditByModel[0].Credits != 6 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", s.CreditByModel[0])
	}
	if s.CreditByModel[1].Rate != "0.5" || s.CreditByModel[1].Credits != 3 || s.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", s.CreditByModel[1])
	}
}

// 同名模型跨域不合并：cn / global 的倍率与实扣是两笔账，行键带域前缀（面板「按模型」
// 列显示的就是 cn:/global:）。此前按裸模型名聚合，两域被并成一行、也看不出行属哪个域。
func TestCreditModelRowsSplitByRealm(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now, "global", "g1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByModel) != 2 {
		t.Fatalf("credit_by_model = %+v, want 2 rows（同倍率的 cn / global 也不合并）", s.CreditByModel)
	}
	got := map[string]float64{}
	for _, row := range s.CreditByModel {
		got[row.Key] = row.Credits
	}
	if got["cn:glm-5.2"] != 1 || got["global:glm-5.2"] != 2 {
		t.Fatalf("rows = %+v, want cn:glm-5.2=1 global:glm-5.2=2", s.CreditByModel)
	}
}

// 旧桶缺倍率时由当前目录倍率回填，并与新桶同倍率记录合并；目录缺失时保留未知行。
func TestCreditLegacyRateFallback(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	s := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.79" ||
		s.CreditByModel[0].Credits != 3 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", s.CreditByModel)
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// 显式区间（「今天」/「自定义」）与滚动窗口走同一套全口径过滤；区间是闭区间
// （桶起点落在 [From, To] 内即命中），且 From/To 会回显给面板确认口径。
func TestSnapshotExplicitWindow(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m",
			Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// 只取中间两小时（base+2h、base+3h）。
	s := r.SnapshotWindow(Window{
		From: base.Add(2 * time.Hour),
		To:   base.Add(3 * time.Hour),
	}, nil, nil)
	if s.Totals.Requests != 2 || s.Totals.PromptTokens != 20 {
		t.Fatalf("显式区间 totals = %d/%d, want 2/20", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 2 || s.Buckets != 2 {
		t.Fatalf("显式区间 series/buckets = %d/%d, want 2/2", len(s.Series), s.Buckets)
	}
	if s.WindowFrom == "" || s.WindowTo == "" {
		t.Fatalf("显式区间应回显 window_from/window_to: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.WindowFrom); err != nil {
		t.Fatalf("window_from 不是 RFC3339: %q", s.WindowFrom)
	}

	// 只有 From（「今天」的形态）：从该点起到最新，全量命中。
	only := r.SnapshotWindow(Window{From: base.Add(4 * time.Hour)}, nil, nil)
	if only.Totals.Requests != 2 {
		t.Fatalf("仅 From 的 totals = %d, want 2", only.Totals.Requests)
	}
	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("仅 From 时 window_to 应为空: %+v", only)
	}

	// 空窗口（From/To 全零且 Hours<=0）= 全部历史，与 Snapshot(0) 等价。
	all := r.SnapshotWindow(Window{}, nil, nil)
	if all.Totals.Requests != 6 {
		t.Fatalf("全零窗口 totals = %d, want 6（全部历史）", all.Totals.Requests)
	}
	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("全部历史不应回显区间: %+v", all)
	}
}

// 滚动窗口的上限仍是 60 天，且与显式区间互不干扰（From/To 优先）。
func TestWindowBounds(t *testing.T) {
	// From/To 优先于 Hours。
	from := time.Now().Add(-2 * time.Hour)
	gotFrom, gotTo := Window{Hours: 720, From: from}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From 应优先于 Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// 只有 Hours：起点 = 当前整点往回 Hours-1 小时。
	f, to := Window{Hours: 24}.bounds()
	want := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/零值", f, to, want)
	}
	// Hours<=0 且无 From/To = 全部历史。
	if f, to := (Window{}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("空窗口 bounds = %v/%v, want 零值/零值", f, to)
	}
	// 上限 60 天。
	f60, _ := Window{Hours: 100000}.bounds()
	want60 := time.Now().Truncate(time.Hour).Add(-(24*60 - 1) * time.Hour)
	if !f60.Equal(want60) {
		t.Fatalf("超限 Hours 未被夹到 60 天: %v want %v", f60, want60)
	}
}

// 脏 scope（解析失败）不进任何口径，也不会让整次快照失败。
func TestBucketTimeRejectsGarbage(t *testing.T) {
	if _, ok := bucketTime("h:not-a-time"); ok {
		t.Fatal("脏小时 scope 应判定失败")
	}
	if _, ok := bucketTime("d:2026-13-45"); ok {
		t.Fatal("脏日 scope 应判定失败")
	}
	if ts, ok := bucketTime("h:2026-09-30T13"); !ok || ts.Hour() != 13 {
		t.Fatalf("合法小时 scope 解析失败: %v %v", ts, ok)
	}
	if ts, ok := bucketTime("d:2026-09-30"); !ok || ts.Day() != 30 {
		t.Fatalf("合法日 scope 解析失败: %v %v", ts, ok)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// 积分历史：扣费积分按桶累计、进聚合（卡片与按小时表的来源），
// 且小时桶折叠成日桶时不能把积分丢掉（丢了就是"看历史漏掉大半"）。
// 没观测到 credit 的请求（HasCredit=false）不参与——"没测到"≠"没花钱"。
func TestCreditsAggregateAndSurviveRollup(t *testing.T) {
	r := New("")
	now := time.Now()
	old := now.AddDate(0, 0, -100) // 超出 90 天小时保留 → 会被折叠成日桶

	r.Add(now, "cn", "u", "m", Delta{TotalTokens: 100, HasTotal: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(now, "cn", "u", "m", Delta{TotalTokens: 100, HasTotal: true, Credit: 0.5, HasCredit: true}, true)
	r.Add(now, "cn", "u", "m", Delta{TotalTokens: 100, HasTotal: true}, true) // 没观测到 credit：不计
	r.Add(old, "cn", "u", "m", Delta{TotalTokens: 200, HasTotal: true, Credit: 4, HasCredit: true}, true)

	win := r.Snapshot(24, nil)
	if win.Totals.Credits != 2 {
		t.Fatalf("窗口内积分 = %v, want 2（未观测到的不计）", win.Totals.Credits)
	}
	if len(win.Series) != 1 || win.Series[0].Credits != 2 {
		t.Fatalf("窗口内小时点积分 = %+v, want 2", win.Series)
	}

	r.Rollup(now)
	all := r.Snapshot(0, nil)
	if all.Totals.Credits != 6 {
		t.Fatalf("折叠后全历史积分 = %v, want 6（4 分来自日桶）", all.Totals.Credits)
	}
	for _, p := range all.Series {
		if p.Scope == "day" && p.Credits != 4 {
			t.Fatalf("日点积分 = %v, want 4", p.Credits)
		}
	}
}

// 今日积分：按本地自然日（不是滚动窗口）汇总各账号实测扣费，小时桶与已折叠的日桶
// 一视同仁；几天前的桶、脏 scope 都不进这一天。
//
// Observed 必须一起断言：当天有桶只说明"有请求"，不说明"测到了扣费"。u3 那种
// 有 token 无 credit 的账号要 Observed=false（花费未知，调用方显示「—」），
// 而 u1 混合了有/无回执的桶，只累加有回执的那几笔且 Observed=true。
func TestTodayCreditsByUID(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.Local)
	r := New("")

	r.Add(now, "cn", "u1", "m", Delta{Credit: 1.5, HasCredit: true}, true)
	r.Add(now.Add(-2*time.Hour), "cn", "u1", "m2", Delta{Credit: 0.5, HasCredit: true}, true)
	r.Add(now, "global", "u2", "m", Delta{Credit: 3, HasCredit: true}, true)
	// 当天有请求但没观测到 credit：出现在结果里，Credits=0 且 Observed=false。
	r.Add(now, "cn", "u3", "m", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	// 同一个 uid 既有回执又有无回执的桶：Observed=true，只加有回执的那笔。
	r.Add(now, "cn", "u4", "m", Delta{Credit: 0.75, HasCredit: true}, true)
	r.Add(now, "cn", "u4", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	// 昨天：排除。
	r.Add(time.Date(2026, 10, 25, 23, 0, 0, 0, time.Local), "cn", "u1", "m", Delta{Credit: 99, HasCredit: true}, true)

	// 已折叠的日桶（Rollup 产物，Add 造不出来）与脏 scope 直接塞进桶表。
	// 日桶带上 CRN=1：Rollup 会把源桶的 CRN 一起累加，构造时漏了它就等于伪造
	// 一个"没观测到"的桶。
	r.mu.Lock()
	r.buckets["d:2026-10-26|cn|u1|m|"] = &bucket{Scope: "d:2026-10-26", Realm: "cn", UID: "u1", Model: "m", CR: 2, CRN: 1}
	r.buckets["h:garbage|cn|u9|m|"] = &bucket{Scope: "h:garbage", Realm: "cn", UID: "u9", Model: "m", CR: 1000}
	r.mu.Unlock()

	got := r.TodayCreditsByUID(now)
	want := map[string]DayCredits{
		"u1": {Credits: 4, Observed: true},  // 1.5 + 0.5(小时) + 2(已折叠日桶)
		"u2": {Credits: 3, Observed: true},  //
		"u3": {Credits: 0, Observed: false}, // 有请求、零回执 → 花费未知
		"u4": {Credits: 0.75, Observed: true},
	}
	if len(got) != len(want) {
		t.Fatalf("today = %v, want %v", got, want)
	}
	for uid, w := range want {
		v, ok := got[uid]
		if !ok || v != w {
			t.Fatalf("today[%s] = %+v(存在=%v), want %+v；全量 %v", uid, v, ok, w, got)
		}
	}
	if _, ok := got["u9"]; ok {
		t.Fatalf("脏 scope 不该计入: %v", got)
	}

	// 换一天看：昨天只有 u1 的那笔。
	yday := r.TodayCreditsByUID(now.AddDate(0, 0, -1))
	if len(yday) != 1 || yday["u1"] != (DayCredits{Credits: 99, Observed: true}) {
		t.Fatalf("昨天 = %v, want {u1:{99 true}}", yday)
	}

	// 全部桶都零回执的日子：条目仍在（有请求），但 Observed=false。
	// 用一个只有 u3 那种桶的日期验证，防止实现退化成"有桶就算观测到"。
	zeroDay := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	r.Add(zeroDay, "cn", "u5", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	if got := r.TodayCreditsByUID(zeroDay); len(got) != 1 || got["u5"] != (DayCredits{Credits: 0, Observed: false}) {
		t.Fatalf("零回执日 = %v, want {u5:{0 false}}", got)
	}
}

// 前缀判日的边界：实现靠「scope 日期段定宽」把字符串比较当成日历日比较，任何
// 近失形状都不能被误判成今天——否则脏 scope 会把别的日子的积分算进「今日已用」。
func TestTodayCreditsByUIDPrefixNearMiss(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.Local)
	r := New("")

	r.mu.Lock()
	// 应计入：今天的合法小时桶（任意小时）与合法日桶。带 CRN 表示真拿到过回执。
	r.buckets["h:2026-10-26T00|cn|hit|m|"] = &bucket{Scope: "h:2026-10-26T00", Realm: "cn", UID: "hit", Model: "m", CR: 0.25, CRN: 1}
	r.buckets["h:2026-10-26T23|cn|hit|m|"] = &bucket{Scope: "h:2026-10-26T23", Realm: "cn", UID: "hit", Model: "m", CR: 0.5, CRN: 1}
	r.buckets["d:2026-10-26|cn|hit|m|"] = &bucket{Scope: "d:2026-10-26", Realm: "cn", UID: "hit", Model: "m", CR: 0.75, CRN: 1}
	// 不应计入：日期段少位 / 多位 / 缺 T / T 换成别的字符 / 日桶带时刻 / 其它前缀。
	misses := []string{
		"h:2026-10-2",      // 少一位（旧年月份个位数形态的脏数据）
		"d:2026-10-2",      // 同上，日桶
		"h:2026-10-260",    // 日期段多一位
		"d:2026-10-260",    // 同上，日桶
		"h:2026-10-2619",   // 日期段吞进了小时
		"h:2026-10-26X10",  // 分隔符不是 T
		"h:2026-10-26T1",   // 小时只有一位
		"h:2026-10-26T100", // 小时三位
		"h:2026-10-26T25",  // 小时越界（ParseInLocation 同样会拒）
		"h:2026-10-26T9a",  // 小时非数字
		"d:2026-10-26T10",  // 日桶带了时刻
		"w:2026-10-26T10",  // 未知前缀
		"h:2026-10-27T10",  // 明天
		"h:2026-10-25T10",  // 昨天
	}
	for i, s := range misses {
		r.buckets[s+"|cn|miss"+string(rune('a'+i))+"|m|"] = &bucket{
			Scope: s, Realm: "cn", UID: "miss" + string(rune('a'+i)), Model: "m", CR: 100,
		}
	}
	r.mu.Unlock()

	got := r.TodayCreditsByUID(now)
	if len(got) != 1 {
		t.Fatalf("today = %v, want 只有 hit 一行（近失 scope 全部排除）", got)
	}
	if v := got["hit"]; v != (DayCredits{Credits: 1.5, Observed: true}) {
		t.Fatalf("hit = %+v, want {1.5 true}（0.25+0.5+0.75，任意小时都算今天）", v)
	}
}

// 无桶当天不等同于「0 积分」：调用方靠 nil 决定省略字段（前端显示「—」）。
// 同时确认全是他日桶时连结果 map 都不分配——惰性 make 就是为了这个。
//
// 阈值是「恰好 1 个分配」，不能更松：真正要挡的对手是"先 make 一个 map 再遍历"
// 的急切实现，实测惰性 = 1.00 alloc/op（只有 day 字符串那一次）、急切 = 2.00
// （多出 map 本体）。任何 >=2 的阈值都放它过去——旧的 >2 阈值就是这样的空断言
// （连急切实现都能量到 2 而通过）。种子堆到 1000 个他日桶：惰性实现仍恒为 1，
// 与桶数无关；急切实现恒为 2，与桶数也无关，所以这个阈值是稳定判据。
// 断言放在闭包外：闭包里的 panic 会被 AllocsPerRun 当成本轮结果，量不出问题。
func TestTodayCreditsByUIDAllocIsPerHitUID(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.Local)
	r := New("")
	r.mu.Lock()
	for i := range 1000 {
		ts := time.Date(2026, 10, 25, 23, 0, 0, 0, time.Local).Add(-time.Duration(i) * time.Hour)
		scope := "h:" + ts.Format(hourLayout)
		r.buckets[scope+"|cn|u1|m|"] = &bucket{Scope: scope, Realm: "cn", UID: "u1", Model: "m", CR: 1, CRN: 1}
	}
	r.mu.Unlock()

	var got map[string]DayCredits
	allocs := testing.AllocsPerRun(50, func() {
		got = r.TodayCreditsByUID(now)
	})
	if got != nil {
		t.Fatalf("他日桶不该产生结果，得到 %v", got)
	}
	if allocs > 1 { // 唯一允许的分配：day 字符串；多一个就是给结果 map 预分配了
		t.Fatalf("无命中时应恰好 1 alloc（day 字符串），实测 %v allocs/op", allocs)
	}
}

// 基准：桶数从 1 千涨到 4 万，今日只命中 1 个 uid。
// 关注点不是绝对耗时，而是 allocs/op 与桶数无关——只有命中 uid 的 map 会分配，
// 绝不随桶数增长（这正是改成锁内单遍遍历的目的）。
func BenchmarkTodayCreditsByUID(b *testing.B) {
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.Local)
	for _, n := range []int{1_000, 10_000, 40_000} {
		b.Run(fmt.Sprintf("buckets=%d", n), func(b *testing.B) {
			r := New("")
			r.mu.Lock()
			for i := range n - 1 {
				// 全部落在历史日：模拟长期运行后的桶表，今天只剩下面手动加的那一个桶。
				ts := time.Date(2026, 10, 25, 23, 0, 0, 0, time.Local).Add(-time.Duration(i) * time.Hour)
				scope := "h:" + ts.Format(hourLayout)
				r.buckets[scope+"|cn|u1|m|"] = &bucket{Scope: scope, Realm: "cn", UID: "u1", Model: "m", CR: 0.01, CRN: 1}
			}
			r.buckets["h:"+now.Format(hourLayout)+"|cn|u1|m|"] = &bucket{
				Scope: "h:" + now.Format(hourLayout), Realm: "cn", UID: "u1", Model: "m", CR: 1.5, CRN: 1,
			}
			b.Logf("bucket count = %d", len(r.buckets))
			r.mu.Unlock()

			b.ReportAllocs()
			for b.Loop() {
				// != 而不是 <：一个把 39 999 个历史桶也算进来的实现会给出 401.5，
				// 用 < 保证不了任何东西。
				if got := r.TodayCreditsByUID(now); got["u1"] != (DayCredits{Credits: 1.5, Observed: true}) {
					b.Fatalf("u1 = %+v", got["u1"])
				}
			}
		})
	}
}

// 没有当天的桶 → nil（调用方据此让 overview 省略字段）；nil 记录器同样返回 nil
// 而不是 panic（Config.Usage 可以为空）。
func TestTodayCreditsByUIDEmptyAndNil(t *testing.T) {
	var nilRec *Recorder
	if got := nilRec.TodayCreditsByUID(time.Now()); got != nil {
		t.Fatalf("nil 记录器应返回 nil，得到 %v", got)
	}
	now := time.Now()
	r := New("")
	r.Add(now.AddDate(0, 0, -3), "cn", "u1", "m", Delta{Credit: 1, HasCredit: true}, true)
	if got := r.TodayCreditsByUID(now); got != nil {
		t.Fatalf("无当天桶应返回 nil，得到 %v", got)
	}
}

// 日界按宿主本地日解析：调用方传进来的是 UTC 也罢，now.In(time.Local) 先把
// 它归一到本地日再判 scope。写成 now.Format 的话，东八区服务器上的 UTC 调用方
// 会把「今天」整整错开一天（本地 00:30 的桶在 UTC 里是前一天 16:30）。
func TestTodayCreditsByUIDNormalizesToLocalDay(t *testing.T) {
	local := time.Date(2026, 10, 26, 0, 30, 0, 0, time.Local)
	utc := local.UTC()
	if utc.Format(dayLayout) == local.Format(dayLayout) {
		t.Skipf("本机时区 %v 下 UTC 与本地同日，测不出归一化", time.Local)
	}

	r := New("")
	r.Add(local, "cn", "u1", "m", Delta{Credit: 2.5, HasCredit: true}, true)
	// 同一时刻的 UTC 视角：必须仍归到本地那一天，取到同一笔。
	got := r.TodayCreditsByUID(utc)
	if v, ok := got["u1"]; !ok || v != (DayCredits{Credits: 2.5, Observed: true}) {
		t.Fatalf("UTC now 未归一到本地日：today(%s UTC) = %v, want u1={2.5 true}", utc.Format(time.RFC3339), got)
	}
}

// 非有限 credit（NaN/±Inf）必须被丢弃：CR 一旦变 NaN，每次 json.Marshal 都失败，
// 而 panel.writeJSON 不看这个错误 → overview/usage 返回 200 空 body，整块面板
// 数字全灭。这里同时确认请求数照记、桶仍可序列化。
func TestAddDropsNonFiniteCredit(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.Local)
	r.Add(now, "cn", "u1", "m", Delta{TotalTokens: 100, HasTotal: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{TotalTokens: 100, HasTotal: true, Credit: math.NaN(), HasCredit: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{Credit: math.Inf(1), HasCredit: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{Credit: math.Inf(-1), HasCredit: true}, true)

	key := "h:" + now.Format(hourLayout) + "|cn|u1|m|"
	r.mu.Lock()
	b := r.buckets[key]
	r.mu.Unlock()
	if b == nil {
		t.Fatalf("桶缺失，键 %q 不在 %d 个桶里", key, len(r.buckets))
	}
	if b.CR != 1.5 || b.CRN != 1 || b.CRT != 100 {
		t.Fatalf("CR/CRN/CRT = %v/%v/%v, want 1.5/1/100（非有限值必须整笔丢弃）", b.CR, b.CRN, b.CRT)
	}
	if b.Req != 4 {
		t.Fatalf("Req = %d, want 4（请求数照记，只有积分记账被挡）", b.Req)
	}
	if _, err := json.Marshal(b); err != nil {
		t.Fatalf("桶仍应可序列化: %v", err)
	}
	if got := r.TodayCreditsByUID(now); got["u1"] != (DayCredits{Credits: 1.5, Observed: true}) {
		t.Fatalf("today = %+v, want u1={1.5 true}", got)
	}
}
