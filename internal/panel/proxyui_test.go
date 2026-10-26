package panel

// 代理池前端回归（node vm 沙箱，照 frontend_test.go / import_paused_test.go 的加载方式；
// 无 node 环境时跳过）。帮助函数加 proxy 前缀，避免与其他测试文件的符号冲突。
//
//	① TestProxyUIBindDialog         绑定弹窗渲染：直连 + 启用节点 radio（停用节点不得进列表），
//	                                单号标题显示昵称；
//	② TestProxyUIAccountsProxyCol   账号池「代理」列：表头/数据行/空态 colspan 三方一致，
//	                                且覆盖 直连 / tag 小标签 / 已停用警示 三态；
//	③ TestProxyUIBatchBindLoop      批量绑定对每个勾选 uid 各调一次 bind_proxy（fetch 计数），
//	                                空 proxy_id = 解绑为直连；
//	④ TestProxyUIUnixTimeFields     B1 回归：last_sync_at / checked_at 是 unix 秒，真 ago/agoUnix
//	                                注入沙箱后整页仍渲染（修复前 ago() 抛 TypeError → 两表全空）；
//	⑤ TestProxyUIBindPreselect      B2 回归：单号按当前 proxy_id 预选、多号一致预选、不一致不预选；
//	⑥ TestProxyUIBindUnbindConfirm  B2 回归：改直连前 confirm 说明解绑账号数，取消则不请求；
//	⑦ TestProxyUIBindFetchError     S4 回归：节点列表拉取失败时弹窗内报错且保留旧缓存渲染；
//	⑧ TestProxyUISingleNodeActions  单节点 test/toggle/delete 的请求形状；
//	⑨ TestProxyUISelGhostCleanup    proxySel 幽灵 id 在重渲染时被清理；
//	⑩ TestProxyUIBatchBusyGuard     批量动作 in-flight 守卫：重复点击只提交一次；
//	⑪ TestProxyUIActionGridAuto     S2：新表用 .acts-inner.auto 自适应列，账号表保持定宽槽位。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// proxyUIRunNode 把 script 写入临时 .cjs 并以 node 执行；工作目录 = internal/panel
// （脚本按 import_paused_test.go 的方式用相对路径读 app.js / index.html）。
func proxyUIRunNode(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; proxy ui test skipped")
	}
	path := filepath.Join(t.TempDir(), "proxyui.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("proxy ui test failed: %v\n%s", err, out)
	}
}

// proxyUIHarness 各用例共用的沙箱骨架：按 id 发放 DOM 桩、fetch 桩计数
// （返回契约形状的 JSON，含 unix 秒时间戳），真 api() / esc() / relFromMs() / ago() /
// agoUnix() 从 app.js 原样切片注入 —— 时间字段的格式化必须由**真实现**决定，
// 用替身把 B1（ago 收到 int64 秒就抛 TypeError）藏起来是本文件原来的缺陷。
const proxyUIHarness = `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');

const els = {};
function el(id) {
  if (!els[id]) els[id] = {
    id, innerHTML: '', textContent: '', hidden: false, value: '', disabled: false,
    classList: { add() {}, remove() {} }, focus() {},
  };
  return els[id];
}
const calls = [];           // fetch 调用（{url, method, body}）+ toast / loadOverview / confirm 事件
let pickValue = null;       // 弹窗里被选中的 radio 值；null = 一个都没选
let confirmAnswer = true;   // confirm 桩的答案（用例可改）
let failPath = null;        // 命中该子串的请求返回 500（验证错误提示）

const ctx = {
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Set, Map, RegExp, Error,
  encodeURIComponent,
  LS_KEY: 'wb2api.key',
  localStorage: { getItem: () => null, setItem() {} },
  document: { querySelector: () => (pickValue == null ? null : { value: pickValue }) },
  confirm: msg => { calls.push({ confirm: msg }); return confirmAnswer; },
  remarkOf: () => '',
  // u1/u2 绑 p1，u3 绑 p2 —— 覆盖「单号预选 / 多号一致 / 多号不一致」三种场景。
  overviewData: { accounts: [
    { uid: 'u1', nickname: '测试号', proxy_id: 'p1', proxy_tag: 'HK-01', proxy_ok: true },
    { uid: 'u2', nickname: 'B', proxy_id: 'p1', proxy_tag: 'HK-01', proxy_ok: true },
    { uid: 'u3', nickname: 'C', proxy_id: 'p2', proxy_tag: 'JP-01', proxy_ok: true },
  ] },
  toast: (msg, cls) => calls.push({ toast: msg, cls: cls || '' }),
  loadOverview: quiet => calls.push({ loadOverview: quiet }),
  $: id => el(id),
};
// 与后端一致：subs.last_sync_at / proxies.checked_at 是 **unix 秒**（int64），不是 ISO。
ctx.fetch = (url, opts) => {
  const u = String(url);
  const method = (opts && opts.method) || 'GET';
  calls.push({ url: u, method: method, body: (opts && opts.body) || '' });
  if (failPath && u.includes(failPath)) {
    return Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({ error: 'boom' }) });
  }
  let data = { ok: true };
  if (u.endsWith('/proxies') && method === 'GET') data = { ok: true, proxies: [
    { id: 'p1', tag: 'HK-01', type: 'ss', server: '1.2.3.4', port: 8388, sub_id: '', enabled: true, latency_ms: 120, checked_at: 1780000000, fails: 0, accounts: 2 },
    { id: 'p2', tag: 'JP-01', type: 'vmess', server: '5.6.7.8', port: 443, sub_id: 's1', enabled: true, latency_ms: 0, checked_at: 0, fails: 0, accounts: 0 },
    { id: 'p3', tag: 'OFF-01', type: 'ss', server: '9.9.9.9', port: 80, sub_id: '', enabled: false, latency_ms: 0, checked_at: 0, fails: 3, accounts: 1 },
  ], subs: [{ id: 's1', name: '机场A', url: 'https://example.com/sub', last_sync_at: 1780000000, count: 1 }] };
  else if (u.endsWith('/test_sel')) data = { ok: true, results: [{ id: 'p1', ok: true, latency_ms: 90 }] };
  else if (u.endsWith('/test')) data = { ok: true, latency_ms: 88 };
  else if (u.endsWith('/toggle')) data = { ok: true, enabled: false };
  else if (u.includes('/bind_proxy')) data = { ok: true, proxy_tag: 'HK-01' };
  return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(data) });
};

// 真 api() 切片 + 真时间格式化（esc / relFromMs / ago / agoUnix）+ 代理池模块
// （模块状态锚点起直到文件尾）。
const apiStart = src.indexOf('async function api('), apiEnd = src.indexOf('function toast(');
const escStart = src.indexOf('function esc('), escEnd = src.indexOf('function relFromMs(');
const timeStart = src.indexOf('function relFromMs('), timeEnd = src.indexOf('function dur(');
const pxStart = src.indexOf('let proxyData = []');
assert.ok(apiStart >= 0 && apiEnd > apiStart, 'api() slice not found');
assert.ok(escStart >= 0 && escEnd > escStart, 'esc() slice not found');
assert.ok(timeStart >= 0 && timeEnd > timeStart, 'relFromMs/ago slice not found');
assert.ok(pxStart > apiEnd, 'proxy module slice not found');
vm.createContext(ctx);
vm.runInContext(src.slice(apiStart, apiEnd), ctx);
vm.runInContext("'use strict';\n" + src.slice(escStart, escEnd) + src.slice(timeStart, timeEnd) +
  '\nthis.esc = esc; this.ago = ago; this.agoUnix = agoUnix;', ctx);
vm.runInContext("'use strict';\n" + src.slice(pxStart), ctx);
// 模块内 let 声明只存在于全局词法环境，用访问器把它们捞出来（幽灵 id 用例要读 proxySel）。
vm.runInContext('this.selOf = () => Array.from(proxySel); this.setSel = ids => { proxySel.clear(); ids.forEach(i => proxySel.add(i)); };' +
  ' this.lastOf = () => proxyLast;', ctx);
`

func TestProxyUIBindDialog(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.openProxyBind(['u3']);   // u3 绑的是 p2
  const list = els['proxyBindList'].innerHTML;
  assert.ok(list.includes('name="pbpick"'), '应渲染 pbpick 单选组: ' + list);
  assert.ok(list.includes('value=""'), '应有「直连」选项');
  assert.ok(list.includes('直连（不使用代理）'), '直连文案应出现');
  assert.ok(list.includes('value="p1"') && list.includes('HK-01'), '启用节点 p1 应进列表: ' + list);
  assert.ok(list.includes('value="p2"'), '启用节点 p2 应进列表');
  assert.ok(!list.includes('value="p3"'), '停用节点不得进绑定列表');
  assert.ok(list.includes('1.2.3.4:8388'), '节点行应带 server:port');
  assert.ok(list.includes('已绑定 2 个'), '节点行应带已绑定数');
  assert.strictEqual(els['proxyVeilTitle'].textContent, '为 C 绑定代理',
    '单号标题应显示昵称，得到 ' + els['proxyVeilTitle'].textContent);
  console.log('proxy bind dialog render passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

func TestProxyUIAccountsProxyCol(t *testing.T) {
	proxyUIRunNode(t, `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');

// 控件齐全：工具条按钮 + 绑定弹窗 + 代理池表（少一个 id 的症状是「按钮点了没反应」）。
for (const id of ['btnBindProxy', 'proxyVeil', 'proxyVeilTitle', 'proxyBindList', 'proxyBindErr',
  'proxyBody', 'proxyPickAll', 'subBody', 'btnSubAdd', 'btnProxyAdd']) {
  assert.ok(html.includes('id="' + id + '"'), 'index.html 缺少 #' + id);
}

// renderAccounts 行渲染切片（照 import_paused_test.go 的加载方式）。
const start = src.indexOf('function renderAccounts('), end = src.indexOf('async function loadOverview(');
assert.ok(start >= 0 && end > start, 'renderAccounts not found');
const tb = { innerHTML: '' };
const ctx = {
  String, Number, Math, Date, Object, Array, JSON, RegExp, console,
  $: id => (id === 'accBody' ? tb : null),
  esc: s => String(s == null ? '' : s), dur: () => '1m', ago: () => '刚刚',
  remarkName: (uid, html2) => html2,
  rateLimitRowsHtml: () => '', formatTokenCount: () => '0', formatLatency: () => '-', formatRate: () => '-',
  disabledKind: () => null, disabledAge: () => '',
  // renderAccounts 走上游的 setHTML 增量重绘：该 helper 在切片之外，给等效实现。
  setHTML: (el, html) => { el.innerHTML = html; },
  // 切片里带着 renderModelLocks，它调切片外的 fmtLocalDateTime；提前注入，免得
  // 将来在这个上下文里用上模型锁渲染时报 ReferenceError。
  fmtLocalDateTime: x => String(x),
};
vm.createContext(ctx);
// 「今日已用」列依赖 fmtCredit（切片外）：求值真实实现，避免复制格式化逻辑。
const helperSrc = src.slice(src.indexOf('function trimFixed'), src.indexOf('function fmtCreditRatio'));
// 锚点缺失会退化成 slice(-1, n) / slice(start, -1) 的垃圾尾巴，静默把测试测空。
assert.ok(helperSrc.includes('function fmtCredit'), 'fmtCredit not found');
vm.runInContext(src.slice(start, end) + helperSrc + '\nthis.renderAccounts = renderAccounts;', ctx);

// 表头列数 = 数据行格数（两行）/2（新增「代理」列必须同步，否则整表错位）。
const bodyIdx = html.indexOf('id="accBody"');
assert.ok(bodyIdx > 0, 'index.html 缺少 accBody');
const headStart = html.lastIndexOf('<thead>', bodyIdx);
const headEnd = html.indexOf('</thead>', headStart);
assert.ok(headStart > 0 && headEnd > headStart, 'accBody 前应有 thead');
const thCount = (html.slice(headStart, headEnd).match(/<th[\s>]/g) || []).length;
assert.strictEqual(thCount, 12, '账号表应有 12 列（含代理、今日已用、每日限额；在途并入成功/失败），得到 ' + thCount);
ctx.renderAccounts([{ uid: 'b', credits: 5, credits_total: 10 }, { uid: 'a', credits: 9, credits_total: 10 }]);
const tdCount = (tb.innerHTML.match(/<td/g) || []).length / 2;
assert.strictEqual(tdCount, thCount, '账号表列数不一致：thead ' + thCount + ' vs 数据行 ' + tdCount);
assert.ok(tb.innerHTML.includes('data-a="proxy"'), '行内应有「代理」按钮');

// 未绑定 → 灰字「直连」。
ctx.renderAccounts([{ uid: 'a', credits: 5, credits_total: 10 }]);
assert.ok(tb.innerHTML.includes('proxy-direct'), '未绑定应渲染 proxy-direct');
assert.ok(tb.innerHTML.includes('直连'), '未绑定应显示「直连」');
assert.ok(!tb.innerHTML.includes('proxy-tag'), '未绑定不应渲染 proxy-tag');

// 绑定且有效 → 节点名小标签（无警示字样）。
ctx.renderAccounts([{ uid: 'a', proxy_tag: 'HK-01', proxy_ok: true, credits: 5, credits_total: 10 }]);
assert.ok(tb.innerHTML.includes('class="proxy-tag"'), '绑定后应渲染 proxy-tag');
assert.ok(tb.innerHTML.includes('HK-01'), '应显示节点 tag');
assert.ok(!tb.innerHTML.includes('已停用'), '有效节点不加「已停用」');

// 节点被停用（proxy_ok=false）→ 警示色 + 「已停用」。
// 只承诺前端真能观察到的状态：删除会经回调解绑（proxy_tag 变空 → 直连），
// 所以这里绝不出现「已删除」这类走不到的文案。
ctx.renderAccounts([{ uid: 'a', proxy_tag: 'HK-01', proxy_ok: false, credits: 5, credits_total: 10 }]);
assert.ok(tb.innerHTML.includes('proxy-tag bad'), '停用节点应为警示色');
assert.ok(tb.innerHTML.includes('已停用'), '停用节点应加「已停用」');
assert.ok(!tb.innerHTML.includes('已删除'), '删除会解绑成直连，不应出现「已删除」');
assert.ok(!tb.innerHTML.includes('已失效'), '口径统一为「已停用」，不再用「已失效」');

// 空态 colspan 与表头一致。
ctx.renderAccounts([]);
const col = /colspan="(\d+)"/.exec(tb.innerHTML);
assert.ok(col && Number(col[1]) === thCount, '空态 colspan 应等于表头列数 ' + thCount + '，得到 ' + (col && col[1]));
console.log('proxy accounts column passed');
`)
}

func TestProxyUIBatchBindLoop(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  // 批量：3 个勾选 uid → 标题带个数；手动选 p1 后确认 → 逐 uid 调 bind_proxy。
  pickValue = 'p1';
  await ctx.openProxyBind(['u1', 'u2', 'u3']);
  assert.ok(els['proxyVeilTitle'].textContent.includes('3 个账号'),
    '批量标题应显示账号数，得到 ' + els['proxyVeilTitle'].textContent);
  await els['btnProxyBindSave'].onclick();
  const binds = calls.filter(c => c.url && c.url.includes('/bind_proxy'));
  assert.strictEqual(binds.length, 3, '每个勾选 uid 各调一次 bind_proxy，得到 ' + binds.length);
  assert.deepStrictEqual(binds.map(b => b.url), [
    '/panel/api/accounts/u1/bind_proxy',
    '/panel/api/accounts/u2/bind_proxy',
    '/panel/api/accounts/u3/bind_proxy',
  ], 'bind_proxy 调用对象应逐一对应勾选 uid');
  assert.ok(binds.every(b => b.body === '{"proxy_id":"p1"}'),
    'body 应带选中的 proxy_id: ' + binds.map(b => b.body).join(','));
  assert.ok(calls.some(c => c.loadOverview !== undefined), '完成后应刷新 overview');
  assert.ok(calls.some(c => c.toast && c.toast.includes('3')), '应 toast 汇总成功数');

  // 解绑：选「直连」（value=""）→ body 为空 proxy_id。
  calls.length = 0;
  pickValue = '';
  await ctx.openProxyBind(['u1']);
  await els['btnProxyBindSave'].onclick();
  const unbinds = calls.filter(c => c.url && c.url.includes('/bind_proxy'));
  assert.strictEqual(unbinds.length, 1, '解绑也应逐 uid 调 bind_proxy');
  assert.strictEqual(unbinds[0].body, '{"proxy_id":""}', '解绑 body 应为空 proxy_id');
  assert.ok(calls.some(c => c.toast && c.toast.includes('直连')), '解绑 toast 应说明直连');
  console.log('proxy batch bind loop passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// B1：last_sync_at / checked_at 是 unix 秒。修复前 renderSubs 里的 ago(1780000000) 在
// iso.startsWith 上抛 TypeError，loadProxies 的 catch 吞掉异常 → renderProxies 不执行 →
// 订阅表与节点表全空。本用例断言两表都渲染出内容且时间列是相对时间。
func TestProxyUIUnixTimeFields(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.loadProxies(true);
  const subs = els['subBody'].innerHTML, nodes = els['proxyBody'].innerHTML;
  assert.ok(subs.includes('机场A'), '订阅表应渲染出订阅行（修复前整表为空）: ' + subs);
  assert.ok(nodes.includes('HK-01'), '节点表应渲染出节点行（修复前整表为空）: ' + nodes);
  // last_sync_at=1780000000 是过去时间 → 相对时间文案，而不是 — 或 ISO 原文。
  assert.ok(/[0-9]+ (秒|分钟|小时|天)前/.test(subs), '订阅「最后同步」应为相对时间: ' + subs);
  assert.ok(!subs.includes('Invalid Date'), '不得出现 Invalid Date');
  // checked_at 走 title 里的「最近检测」，同样必须是相对时间。
  assert.ok(nodes.includes('最近检测：') && /最近检测：[0-9]+ (秒|分钟|小时|天)前/.test(nodes),
    '延迟列 title 应带相对检测时间: ' + nodes);
  // 从未测过的节点（checked_at=0）→ 尚未测速，不是 1970 年。
  assert.ok(nodes.includes('尚未测速'), 'checked_at=0 应显示尚未测速');
  // agoUnix 只认 unix 秒；空/0 一律 —。
  assert.strictEqual(ctx.agoUnix(0), '—');
  assert.strictEqual(ctx.agoUnix(null), '—');
  assert.strictEqual(ctx.agoUnix(undefined), '—');
  assert.ok(/前$/.test(ctx.agoUnix(1780000000)), 'unix 秒应格式化为相对时间，得到 ' + ctx.agoUnix(1780000000));
  // 老调用点（last_success 是 ISO）不许被改坏。
  assert.strictEqual(ctx.ago('0001-01-01T00:00:00Z'), '—');
  assert.ok(/前$|刚刚/.test(ctx.ago(new Date(Date.now() - 120000).toISOString())), 'ISO 仍应可解析');
  console.log('proxy unix time fields passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// B2：绑定弹窗必须按当前 proxy_id 预选，否则在已绑定的账号上点确认会静默解绑。
func TestProxyUIBindPreselect(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
function checkedValues(html) {
  const out = [];
  const re = /<input type="radio" name="pbpick" value="([^"]*)"( checked)?>/g;
  let m;
  while ((m = re.exec(html))) if (m[2]) out.push(m[1]);
  return out;
}
(async () => {
  // 单号：u1 当前绑 p1 → 预选 p1，直连不选中。
  await ctx.openProxyBind(['u1']);
  assert.deepStrictEqual(checkedValues(els['proxyBindList'].innerHTML), ['p1'],
    '单号应按 proxy_id 预选，得到 ' + els['proxyBindList'].innerHTML);

  // 多号一致（u1,u2 都绑 p1）→ 预选 p1，且不出现「不一致」提示。
  await ctx.openProxyBind(['u1', 'u2']);
  assert.deepStrictEqual(checkedValues(els['proxyBindList'].innerHTML), ['p1'], '多号一致应预选该节点');
  assert.ok(!els['proxyBindList'].innerHTML.includes('不一致'), '一致时不该提示不一致');

  // 多号不一致（u1 绑 p1、u3 绑 p2）→ 不预选 + 弹窗内提示。
  await ctx.openProxyBind(['u1', 'u3']);
  assert.deepStrictEqual(checkedValues(els['proxyBindList'].innerHTML), [],
    '多号不一致时不得预选，得到 ' + els['proxyBindList'].innerHTML);
  assert.ok(els['proxyBindList'].innerHTML.includes('所选账号当前代理不一致'), '多号不一致应在弹窗内提示');

  // 不一致且用户没选就点保存 → 报错且不发 bind_proxy（否则就是静默解绑）。
  calls.length = 0;
  pickValue = null;
  await els['btnProxyBindSave'].onclick();
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/bind_proxy')).length, 0,
    '未选择时不得提交任何绑定请求');
  assert.strictEqual(els['proxyBindErr'].hidden, false, '未选择时应显示错误提示');
  assert.ok(/请先选择一个节点/.test(els['proxyBindErr'].textContent), '错误文案应提示先选择');

  // 未绑定过的账号 → 预选直连。
  ctx.overviewData.accounts.push({ uid: 'u9', nickname: '新号' });
  await ctx.openProxyBind(['u9']);
  assert.deepStrictEqual(checkedValues(els['proxyBindList'].innerHTML), [''],
    '未绑定账号应预选直连，得到 ' + els['proxyBindList'].innerHTML);

  // 绑定节点已停用（不在启用列表）→ 回落直连并说明「保存即改为直连」。
  ctx.overviewData.accounts.push({ uid: 'u8', nickname: '停用号', proxy_id: 'p3', proxy_tag: 'OFF-01', proxy_ok: false });
  await ctx.openProxyBind(['u8']);
  assert.deepStrictEqual(checkedValues(els['proxyBindList'].innerHTML), [''],
    '已停用节点的账号应回落直连');
  assert.ok(els['proxyBindList'].innerHTML.includes('保存即改为直连'), '应提示保存等于改直连');
  console.log('proxy bind preselect passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// B2：改直连（解绑）是破坏性动作，必须在 confirm 里说清受影响账号数。
func TestProxyUIBindUnbindConfirm(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.openProxyBind(['u1', 'u3']);
  pickValue = '';                       // 明确选择直连
  calls.length = 0;
  confirmAnswer = true;
  await els['btnProxyBindSave'].onclick();
  const conf = calls.filter(c => c.confirm);
  assert.strictEqual(conf.length, 1, '改直连应弹一次 confirm，得到 ' + conf.length);
  assert.ok(/2 个账号解绑/.test(conf[0].confirm), 'confirm 应说明解绑账号数，得到 ' + conf[0].confirm);
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/bind_proxy')).length, 2,
    '确认后应逐 uid 解绑');

  // 取消 → 不发请求。
  calls.length = 0;
  confirmAnswer = false;
  await ctx.openProxyBind(['u1']);
  pickValue = '';
  calls.length = 0;
  await els['btnProxyBindSave'].onclick();
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/bind_proxy')).length, 0,
    'confirm 取消后不得发请求');

  // 未绑定过的账号选直连 → 无事可解绑，不打扰用户。
  calls.length = 0;
  confirmAnswer = true;
  await ctx.openProxyBind(['u9']);
  pickValue = '';
  calls.length = 0;
  await els['btnProxyBindSave'].onclick();
  assert.strictEqual(calls.filter(c => c.confirm).length, 0, '本来就没绑定的账号选直连不该弹 confirm');
  console.log('proxy bind unbind confirm passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// S4：首次打开账号池（缓存为空）时 GET /proxies 失败，必须把失败原因写进弹窗，
// 而不是只显示「没有启用节点」；已有缓存时仍照旧渲染缓存。
func TestProxyUIBindFetchError(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.loadProxies(true);          // 先有缓存
  failPath = 'proxies';
  await ctx.openProxyBind(['u1']);
  assert.strictEqual(els['proxyBindErr'].hidden, false, '拉取失败应显示错误区');
  assert.ok(els['proxyBindErr'].textContent.includes('节点列表拉取失败'),
    '错误区应说明拉取失败，得到 ' + els['proxyBindErr'].textContent);
  assert.ok(els['proxyBindErr'].textContent.includes('boom'), '错误区应带后端 e.message');
  const list = els['proxyBindList'].innerHTML;
  assert.ok(list.includes('HK-01'), '拉取失败仍应渲染旧缓存节点: ' + list);
  assert.ok(!list.includes('当前没有启用状态的节点'), '有缓存时不得误报「没有启用节点」');

  // 打开成功后错误区要清空（不能把上次的失败一直挂着）。
  failPath = null;
  await ctx.openProxyBind(['u1']);
  assert.strictEqual(els['proxyBindErr'].hidden, true, '成功时应隐藏错误区');
  assert.strictEqual(els['proxyBindErr'].textContent, '', '成功时应清空错误文案');
  console.log('proxy bind fetch error passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// S1：单节点 测试 / 启停 / 删除 的请求形状（方法、路径、body 有无）。
func TestProxyUISingleNodeActions(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.loadProxies(true);
  const btn = (act, id) => ({
    dataset: { a: act, p: id },
    closest: sel => (sel === 'button[data-a]' ? { dataset: { a: act, p: id }, disabled: false } : null),
  });
  const click = async (act, id) => {
    calls.length = 0;
    await els['proxyBody'].onclick({ target: btn(act, id) });
    return calls.filter(c => c.url && c.url.includes('/panel/api/proxies/'));
  };
  const test = await click('test', 'p1');
  assert.deepStrictEqual(test.map(c => [c.method, c.url, c.body]),
    [['POST', '/panel/api/proxies/p1/test', '']], '测速应为 POST /proxies/p1/test，得到 ' + JSON.stringify(test));
  assert.ok(calls.some(c => c.toast && c.toast.includes('88')), '测速成功应 toast 延迟');

  const tog = await click('toggle', 'p2');
  assert.deepStrictEqual(tog.map(c => [c.method, c.url, c.body]), [['POST', '/panel/api/proxies/p2/toggle', '']],
    '启停应为 POST /proxies/p2/toggle，得到 ' + JSON.stringify(tog));

  const del = await click('del', 'p3');
  assert.deepStrictEqual(del.map(c => [c.method, c.url, c.body]), [['DELETE', '/panel/api/proxies/p3', '']],
    '删除应为 DELETE /proxies/p3（confirm 已同意），得到 ' + JSON.stringify(del));

  // 删除要 confirm，取消则一个请求都不发。
  calls.length = 0;
  confirmAnswer = false;
  await els['proxyBody'].onclick({ target: btn('del', 'p3') });
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/panel/api/proxies/')).length, 0, '取消删除不得发请求');
  console.log('proxy single node actions passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// S1：勾选集里的幽灵 id（节点已删/订阅重同步）必须在重渲染时清掉，否则
// 「已选 N 个」虚高、批量操作会对着不存在的 id 空转。
func TestProxyUISelGhostCleanup(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.loadProxies(true);
  ctx.setSel(['p1', 'ghost-a', 'ghost-b']);
  assert.deepStrictEqual(ctx.selOf().sort(), ['ghost-a', 'ghost-b', 'p1'], '前置：三个 id 在集合里');
  ctx.renderProxies(ctx.lastOf());
  assert.deepStrictEqual(ctx.selOf(), ['p1'], '重渲染后幽灵 id 应被清理，得到 ' + ctx.selOf().join(','));
  assert.strictEqual(els['proxySelNote'].textContent, '1 / 3 个已选', '表头计数应基于清理后的集合');
  // 全部消失 → 计数清空。
  ctx.setSel(['x']);
  ctx.renderProxies([]);
  assert.deepStrictEqual(ctx.selOf(), [], '空列表应清光勾选');
  assert.strictEqual(els['proxySelNote'].textContent, '', '空列表不应显示计数');
  console.log('proxy sel ghost cleanup passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// N：批量动作 in-flight 守卫。5s 轮询重建 tbody 会让旧按钮的 disabled 失效，
// 只有标志位能挡住重复提交（双击不该把同一批节点 toggle 两遍）。
func TestProxyUIBatchBusyGuard(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  await ctx.loadProxies(true);
  ctx.setSel(['p1', 'p2']);
  calls.length = 0;
  // 同步连点两次：第一次进入后标志位已置位，第二次应被挡下并提示。
  const first = els['btnProxyDisableSel'].onclick();
  const second = els['btnProxyDisableSel'].onclick();
  await Promise.all([first, second]);
  const toggles = calls.filter(c => c.url && c.url.includes('/toggle'));
  assert.strictEqual(toggles.length, 2, '两个已启用节点各 toggle 一次（重复点击不得翻倍），得到 ' + toggles.length);
  assert.ok(calls.some(c => c.toast && c.toast.includes('还在进行中')), '重复点击应提示稍候');

  // 守卫在结束后释放：换个目标再点一次仍能正常提交（p3 是唯一停用节点）。
  ctx.setSel(['p3']);
  calls.length = 0;
  await els['btnProxyEnableSel'].onclick();
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/toggle')).length, 1,
    '守卫释放后「批量启用」应 toggle 停用的 p3，得到 ' + JSON.stringify(calls.map(c => c.url || c.toast)));

  // 绑定保存同样有守卫：连点两次只绑一遍。
  pickValue = 'p1';
  await ctx.openProxyBind(['u1', 'u2']);
  calls.length = 0;
  const a = els['btnProxyBindSave'].onclick();
  const b = els['btnProxyBindSave'].onclick();
  await Promise.all([a, b]);
  assert.strictEqual(calls.filter(c => c.url && c.url.includes('/bind_proxy')).length, 2,
    '连点保存只应绑 2 次（每个 uid 一次），得到 ' + calls.filter(c => c.url && c.url.includes('/bind_proxy')).length);
  console.log('proxy batch busy guard passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}

// S2：订阅表 / 节点表只有 2~3 个按钮，不能复用账号表 8 槽定宽模板（会白占
// 320~380px 把表撑出横向滚动条）。这里做结构断言；版面本身由浏览器按 auto 列排。
func TestProxyUIActionGridAuto(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	css, app := string(html), string(src)

	// .acts-inner.auto：按实际按钮数自动成列（不留空槽），列宽仍统一 54px。
	if !strings.Contains(css, ".acts-inner.auto {") {
		t.Fatal("index.html 缺少 .acts-inner.auto 规则（新表需要自适应列）")
	}
	auto := css[strings.Index(css, ".acts-inner.auto {"):]
	auto = auto[:strings.Index(auto, "}")]
	for _, want := range []string{"grid-auto-flow: column", "grid-auto-columns: 54px", "grid-template-columns: none"} {
		if !strings.Contains(auto, want) {
			t.Fatalf(".acts-inner.auto 缺少 %q：%s", want, auto)
		}
	}
	if strings.Contains(auto, "repeat(") {
		t.Fatalf(".acts-inner.auto 不得继承定宽 8 轨模板：%s", auto)
	}
	// 账号表仍用定宽槽位（互斥动作缺位留空槽，逐行对齐靠它）。
	if !strings.Contains(css, ".acts-inner { display: grid; grid-template-columns: repeat(5, 54px)") {
		t.Fatal("账号表的定宽 .acts-inner 模板不应改动")
	}
	if !strings.Contains(app, `'<td class="acts"><div class="acts-inner">' + [`) {
		t.Fatal("renderAccounts 应继续使用定宽 .acts-inner")
	}
	// 订阅表 2 个按钮、节点表 3 个按钮 → 都用 .acts-inner auto。
	if n := strings.Count(app, `<div class="acts-inner auto">`); n != 2 {
		t.Fatalf("订阅表与节点表都应使用 .acts-inner auto，得到 %d 处", n)
	}
	if !strings.Contains(app, `data-act="sync"`) || !strings.Contains(app, `data-a="toggle"`) {
		t.Fatal("新表按钮应仍是 data-act / data-a 委托口径")
	}
	// 停用节点行加了等价样式（原来 tr.off 对节点表零效果）。
	if !strings.Contains(css, "#proxyBody tr.off td:not(.acts)") {
		t.Fatal("节点表停用行缺少等价 .off 样式")
	}
}
