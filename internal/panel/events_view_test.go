package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestAppJSEventsView 事件日志页的两条硬约束：
//
//  1. 读取失败必须说出来。原来 loadEvents 用 .catch(() => ({entries: []})) 把 501
//     （归档未开启）和网络错误都变成「共 0 条事件」——和「真的没流量」长得一模一样，
//     外层 try/catch 因此成了死代码。
//  2. 表头 13 列与行内 13 个 <td> 必须一致，否则整张表错位。
//
// 两处都是纯前端、都看不见失败，所以用 node 跑真实函数而不是读源码断言。
func TestAppJSEventsView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; events view test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('async function loadEvents() {');
const e = src.indexOf('\n}\n', src.indexOf('function deriveEventExecutor(ua) {'));
if (s < 0 || e < 0) throw new Error('events view block not found');
const body = src.slice(s, e + 3);

// 汇总条（renderEventStats）用到的 fmtMs / fmtNum 定义在切片之外。这里求值真实实现
// 而不是塞 stub：格式化逻辑一旦回归，这个用例应该跟着炸，而不是被假函数蒙过去。
function pickFn(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error(name + ' not found');
  return src.slice(i, src.indexOf('\n}\n', i) + 3);
}
const fmtSrc = pickFn('fmtMs') + pickFn('fmtNum');

function makeCtx(apiImpl) {
  const els = {};
  const el = (k) => (els[k] = els[k] || { textContent: '', value: '', innerHTML: '' });
  // setHTML 的生产实现带 lastHTML 缓存：同内容跳过重绘。这里必须照抄，
  // 否则「清空后再渲染同样内容」这类缓存 bug 在测试里永远看不见。
  const lastHTML = new WeakMap();
  const ctx = {
    rawEventsData: [],
    esc: (x) => String(x == null ? '' : x),
    $: el,
    setHTML: (node, html) => {
      if (lastHTML.get(node) === html) return;
      lastHTML.set(node, html);
      node.innerHTML = html;
    },
    api: apiImpl,
    renderEventTable: undefined,
  };
  vm.createContext(ctx);
  vm.runInContext('let rawEventsData = [];\n' + fmtSrc + body +
    '\nthis.populate = populateEventFilterOptions; this.render = renderEventTable;' +
    ' this.load = loadEvents; this.get = () => rawEventsData;', ctx);
  return { ctx, els, el };
}

async function run() {
  const out = {};
  // 归档未开启：501 要说人话
  const a = makeCtx(async () => { const e = new Error('request logger not available'); e.status = 501; throw e; });
  a.el('eventFilterLimit').value = '100';
  await a.ctx.load();
  out.e501 = a.el('eventTableBody').innerHTML;
  out.badge501 = a.el('eventBadge').textContent;

  // 其它错误：把原始 message 带出来
  const b = makeCtx(async () => { const e = new Error('boom'); e.status = 500; throw e; });
  await b.ctx.load();
  out.e500 = b.el('eventTableBody').innerHTML;

  // 成功：一行一条事件，列数按 <td> 计
  const c = makeCtx(async () => ({ entries: [{
    time: '2026-10-26T10:00:00Z', path: '/v1/chat/completions', model: 'wb-lite',
    account: 'a@b.c (nick)', status: 200, ok: true, duration_ms: 1200, ttfb_ms: 300,
    prompt_tokens: 10, completion_tokens: 20, total_tokens: 30,
    reasoning_tokens: 5, reasoning_effort: 'high', mode: 'stream',
    cache_hit_tokens: 4, cache_miss_tokens: 6, executor_type: 'CodexExecutor',
    client_ip: '10.0.0.1', user_agent: 'codex/1.0'
  }] }));
  await c.ctx.load();
  const html = c.el('eventTableBody').innerHTML;
  out.rows = (html.match(/<tr>/g) || []).length;
  out.cells = (html.match(/<td/g) || []).length / (out.rows || 1);
  out.hasFakeKey = /Gra\*+cle@1!/.test(html);
  out.hasUpstreamLie = html.indexOf('上游响应') >= 0;
  out.cacheMiss = html.indexOf('未命中 6') >= 0;

  // 汇总条：筛到无结果再切回来，必须重新画出来。
  // 回归点：空分支曾经直接 box.innerHTML = ''，绕过 setHTML 的 lastHTML 缓存，
  // 切回非空筛选时被当成「内容没变」跳过重绘，汇总条永久空白。
  const d = makeCtx(async () => ({ entries: [
    { time: '2026-10-26T10:00:00Z', model: 'wb-lite', account: 'a@b.c', status: 200,
      ok: true, duration_ms: 1000, attempts: 1 },
    { time: '2026-10-26T10:00:01Z', model: 'wb-lite', account: 'a@b.c', status: 429,
      ok: false, duration_ms: 800, attempts: 2 }
  ] }));
  d.el('eventFilterLimit').value = '100';
  await d.ctx.load();
  d.el('eventFilterStatus').value = '';
  d.ctx.render();
  out.statsFirst = d.el('evStats').innerHTML;
  out.statsHiddenFirst = !!d.el('evStats').hidden;
  // 用模型筛选造「0 条」：status=error 筛不掉 429（429 也算失败），凑不出空集。
  d.el('eventFilterModel').value = '不存在的模型';
  d.ctx.render();
  out.statsHiddenEmpty = !!d.el('evStats').hidden;
  out.statsEmptyHTML = d.el('evStats').innerHTML;
  d.el('eventFilterModel').value = '';
  d.ctx.render();
  out.statsAgain = d.el('evStats').innerHTML;
  process.stdout.write(JSON.stringify(out));
}
run().catch(e => { console.error(e); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "events-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got struct {
		E501             string  `json:"e501"`
		Badge501         string  `json:"badge501"`
		E500             string  `json:"e500"`
		Rows             int     `json:"rows"`
		Cells            float64 `json:"cells"`
		HasFakeKey       bool    `json:"hasFakeKey"`
		HasUpstreamLie   bool    `json:"hasUpstreamLie"`
		CacheMiss        bool    `json:"cacheMiss"`
		StatsFirst       string  `json:"statsFirst"`
		StatsHiddenFirst bool    `json:"statsHiddenFirst"`
		StatsHiddenEmpty bool    `json:"statsHiddenEmpty"`
		StatsEmptyHTML   string  `json:"statsEmptyHTML"`
		StatsAgain       string  `json:"statsAgain"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if !strings.Contains(got.E501, "请求归档未开启") {
		t.Errorf("501 应当提示归档未开启，实际: %s", got.E501)
	}
	if got.Badge501 != "读取失败" {
		t.Errorf("失败时徽标不能留在旧数字，实际: %q", got.Badge501)
	}
	if !strings.Contains(got.E500, "读取失败: boom") {
		t.Errorf("其它错误要带出原始 message，实际: %s", got.E500)
	}
	for name, html := range map[string]string{"501": got.E501, "500": got.E500} {
		if strings.Contains(html, "共 0 条事件") || strings.Contains(html, "没有符合筛选条件") {
			t.Errorf("%s 被渲染成「没有事件」了: %s", name, html)
		}
	}
	if got.Rows != 1 {
		t.Fatalf("应当渲染 1 行，实际 %d", got.Rows)
	}
	if got.Cells != 13 {
		t.Errorf("行内列数应为 13（与表头一致），实际 %v", got.Cells)
	}
	if got.HasFakeKey || got.HasUpstreamLie {
		t.Errorf("伪造列回来了：假 key=%v 假上游模型=%v", got.HasFakeKey, got.HasUpstreamLie)
	}
	if !got.CacheMiss {
		t.Error("缓存未命中数没有渲染出来")
	}
	// 汇总条：50% 成功率（1 成功 / 1 限流），且筛到无结果再切回必须重绘
	if !strings.Contains(got.StatsFirst, "请求成功率 50.0%") {
		t.Errorf("汇总条成功率应为 50.0%%，实际: %s", got.StatsFirst)
	}
	if got.StatsHiddenFirst {
		t.Error("有数据时汇总条不该隐藏")
	}
	if !got.StatsHiddenEmpty || got.StatsEmptyHTML != "" {
		t.Errorf("无结果时汇总条应隐藏并清空，hidden=%v html=%q", got.StatsHiddenEmpty, got.StatsEmptyHTML)
	}
	if !strings.Contains(got.StatsAgain, "请求成功率 50.0%") {
		t.Errorf("从无结果筛选切回后汇总条没有重绘（setHTML 缓存失效）: %q", got.StatsAgain)
	}
}

// TestEventsTableHeaderColumns 表头列数与行内列数必须同步改。
func TestEventsTableHeaderColumns(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	i := strings.Index(string(raw), `<table class="event-table">`)
	if i < 0 {
		t.Fatal("事件表不存在")
	}
	seg := string(raw)[i:]
	if j := strings.Index(seg, "</thead>"); j > 0 {
		seg = seg[:j]
	}
	n := len(regexp.MustCompile(`<th[ >]`).FindAllString(seg, -1))
	if n != 13 {
		t.Errorf("事件表表头应为 13 列，实际 %d", n)
	}
}
