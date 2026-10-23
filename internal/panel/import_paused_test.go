package panel

// 两条运维侧小功能的回归测试：
//
//	1. 导入（cockpit JSON）进来的号**默认暂停选号**，仍留在池里跑保号任务，
//	   「恢复选号」后立即可选，重复导入同一 uid 也回到暂停态。
//	2. 面板账号池的「按状态排序 + 多选批量暂停/恢复选号」：
//	   index.html 控件齐全、表头与数据行列数一致、勾选态能跨重绘保持、
//	   状态排序顺序为 禁用 → 暂停 → 冷却 → 可用。

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

func TestImportAccountDefaultsPaused(t *testing.T) {
	pl := pool.New("")
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	a := &auth.Auth{
		UID:          "uid-import-1",
		AccessToken:  "at",
		RefreshToken: "rt",
		Nickname:     "导入号",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	if isNew := p.addImportedAccount(a); !isNew {
		t.Fatal("首次导入应视为新号")
	}
	// 核心断言：默认不进转发路径。
	if got := pl.Pick(); got != nil {
		t.Fatalf("导入的号默认必须暂停选号，实际被选中 uid=%s", got.UID)
	}
	// 「待确认」标记（hold）必须落到状态里，供面板渲染「新号待确认」。
	if st, ok := pl.Status("uid-import-1"); !ok || !st.Hold {
		t.Fatalf("导入的号应带 hold 待确认标记，实际 %+v ok=%v", st, ok)
	}
	// 关键回归：解冻（Revive = 全清）不得清掉导入号的暂停，否则一次「解冻」
	// 或任何按 Revive 语义实现的自动路径就把未验证的新号推进了转发路径。
	pl.Revive("uid-import-1")
	if st, _ := pl.Status("uid-import-1"); !st.Paused || !st.Hold {
		t.Fatalf("解冻不得清除导入号的待确认暂停，实际 %+v", st)
	}
	if got := pl.Pick(); got != nil {
		t.Fatalf("解冻后仍不得进选号池，实际被选中 uid=%s", got.UID)
	}
	// 暂停 ≠ 禁用：号还在池里，保号任务（签到/活跃/保活/余额）照常遍历它。
	if got := pl.AuthByUID("uid-import-1"); got == nil {
		t.Fatal("导入后账号应已在池内（暂停只关选号）")
	}
	// 恢复选号是即时的，不需要解冻/重登。
	if !pl.Resume("uid-import-1") {
		t.Fatal("Resume 应成功")
	}
	if pl.Pick() == nil {
		t.Fatal("恢复选号后应能被选中")
	}

	// 重复导入同一 uid：不是新号，但同样回到暂停态——否则"已经导入过"的号会绕过
	// 默认暂停，人一刷新就发现它悄悄进了选号池。
	if isNew := p.addImportedAccount(a); isNew {
		t.Fatal("重复导入同一 uid 不该算新号")
	}
	if pl.Pick() != nil {
		t.Fatal("重复导入后也应停在暂停选号态")
	}
}

func TestAppJSAccountsBatchUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; accounts batch UI test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');

// 1) 控件齐全：排序下拉 + 批量暂停/恢复 + 表头全选。
for (const id of ['accSort', 'btnPauseSel', 'btnResumeSel', 'accPickAll']) {
  assert.ok(html.includes('id="' + id + '"'), 'index.html 缺少 #' + id);
}

const start = src.indexOf('function renderAccounts('), end = src.indexOf('async function loadOverview(');
assert.ok(start >= 0 && end > start, 'renderAccounts not found');
const tb = { innerHTML: '' };
const ctx = {
  String, Number, Math, Date, Object, Array, JSON, RegExp, console,
  $: id => (id === 'accBody' ? tb : null),
  esc: s => String(s == null ? '' : s), dur: () => '1m', ago: () => '刚刚',
  remarkName: (uid, html2) => html2,
  rateLimitRowsHtml: () => '', formatTokenCount: () => '0', formatLatency: () => '-', formatRate: () => '-',
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.renderAccounts = renderAccounts; this.sortAccounts = sortAccounts;' +
  '\nthis.setSort = v => { accSort = v; }; this.pick = u => accSel.add(u);', ctx);

// 2) 表头列数 = 数据行格数（多选列加进来后必须同步，否则整表错位）。
//    注意：页面里有多个 class="acc" 表格（到期清单等），必须锚定 accBody 再往回找 thead。
const bodyIdx = html.indexOf('id="accBody"');
assert.ok(bodyIdx > 0, 'index.html 缺少 accBody');
const headStart = html.lastIndexOf('<thead>', bodyIdx);
const headEnd = html.indexOf('</thead>', headStart);
assert.ok(headStart > 0 && headEnd > headStart, 'accBody 前应有 thead');
// /<th[\s>]/ 而非 /<th/：后者会把 <thead> 自己算进去。
const thCount = (html.slice(headStart, headEnd).match(/<th[\s>]/g) || []).length;
ctx.renderAccounts([{ uid: 'b', credits: 5, credits_total: 10 }, { uid: 'a', credits: 9, credits_total: 10 }]);
const tdCount = (tb.innerHTML.match(/<td/g) || []).length / 2;
assert.strictEqual(tdCount, thCount, '账号表列数不一致：thead ' + thCount + ' vs 数据行 ' + tdCount);
assert.ok(tb.innerHTML.includes('class="pk" data-u="a"'), '数据行应带多选复选框: ' + tb.innerHTML.slice(0, 200));

// 3) 空态 colspan 与表头一致（否则"账号池是空的"会缩在半边）。
ctx.renderAccounts([]);
const col = /colspan="(\d+)"/.exec(tb.innerHTML);
assert.ok(col && Number(col[1]) === thCount, '空态 colspan 应等于表头列数 ' + thCount + '，得到 ' + (col && col[1]));

// 4) 勾选态跨重绘保持（否则排在后面的号一点排序/刷新就掉选）。
ctx.pick('a');
ctx.renderAccounts([{ uid: 'a', credits: 1 }, { uid: 'b', credits: 2 }]);
assert.ok(/class="pk" data-u="a" checked/.test(tb.innerHTML), '已勾选的号重绘后应保持勾选');

// 5) 状态排序：禁用 → 暂停 → 冷却 → 可用；同档按积分降序。
ctx.setSort('state');
ctx.renderAccounts([
  { uid: 'okc', credits: 50 },
  { uid: 'disabled', disabled: true, credits: 10 },
  { uid: 'paused', paused: true, credits: 20 },
  { uid: 'cooling', cool_remaining_sec: 120, credits: 30 },
]);
const order = [...tb.innerHTML.matchAll(/class="pk" data-u="([A-Za-z0-9_-]+)"/g)].map(m => m[1]);
assert.deepStrictEqual(order, ['disabled', 'paused', 'cooling', 'okc'],
  '状态排序应为 禁用→暂停→冷却→可用，得到 ' + order.join(','));

console.log('accounts batch ui passed');`
	path := filepath.Join(t.TempDir(), "accbatch.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("accounts batch ui test failed: %v\n%s", err, out)
	}
}
