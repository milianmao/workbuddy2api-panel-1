package panel

// 订阅编辑回归（node vm 沙箱，用 proxyui_test.go 的 proxyUIRunNode / proxyUIHarness）。
//
// 起因：以前换机场 token 只有「删了重建」一条路，而删订阅会连带移除节点、把绑定这些
// 节点的账号打回随机出口。这里钉住三条前端契约：
//
//	① 订阅行的操作列必须带「编辑」；
//	② 编辑态预填名称/链接，标题与保存按钮文案切换成编辑口径；
//	③ 保存走 PUT /panel/api/subscriptions/{id}，新增仍走 POST。
//
// 后端契约（id 按新链接重算、节点 SubID 迁移、落盘）见 internal/nodebridge/bridge_test.go。

import "testing"

func TestProxyUISubEdit(t *testing.T) {
	proxyUIRunNode(t, proxyUIHarness+`
(async () => {
  const sub = { id: 's1', name: '机场A', url: 'https://example.com/sub?token=1', last_sync_at: 1780000000, count: 1 };

  // ① 行内必须能点到「编辑」，原有同步/删除不能丢
  ctx.renderSubs([sub]);
  const row = els['subBody'].innerHTML;
  assert.ok(row.includes('data-act="edit"'), '订阅行应有编辑按钮: ' + row);
  assert.ok(row.includes('data-act="sync"') && row.includes('data-act="del"'), '同步/删除不该丢: ' + row);

  // ② 编辑态：预填 + 文案切换
  ctx.openSubAdd(sub);
  assert.strictEqual(els['subNameInput'].value, '机场A', '名称应预填');
  assert.strictEqual(els['subUrlInput'].value, sub.url, '链接应预填');
  assert.strictEqual(els['subTitle'].textContent, '编辑订阅', '标题应为编辑订阅');
  assert.strictEqual(els['btnSubCreate'].textContent, '保存修改', '按钮应为保存修改');

  // ③ 保存 → PUT 到带 id 的地址
  await els['btnSubCreate'].onclick();
  const put = calls.filter(c => c.method === 'PUT');
  assert.strictEqual(put.length, 1, '应发一次 PUT: ' + JSON.stringify(calls));
  assert.ok(/\/panel\/api\/subscriptions\/s1$/.test(put[0].url), 'PUT 地址不对: ' + put[0].url);
  const body = JSON.parse(put[0].body);
  assert.strictEqual(body.url, sub.url, 'PUT 应带上链接');
  assert.strictEqual(body.name, '机场A', 'PUT 应带上名称');
  assert.strictEqual(els['btnSubCreate'].disabled, false, '保存后按钮应恢复可用');

  // ④ 事件委托：表里点「编辑」只打开弹窗，不发请求
  await ctx.loadProxies(true);
  calls.length = 0;
  await els['subBody'].onclick({ target: { closest: () => ({ dataset: { s: 's1', act: 'edit' }, disabled: false }) } });
  assert.strictEqual(els['subNameInput'].value, '机场A', '委托打开应预填名称');
  // 委托路径读的是实时列表（loadProxies 的返回），不是本次测试构造的对象。
  assert.ok(els['subUrlInput'].value.includes('example.com/sub'), '委托打开应预填链接，得到 ' + els['subUrlInput'].value);
  assert.strictEqual(calls.filter(c => c.method && c.method !== 'GET').length, 0,
    '打开弹窗不该发写请求: ' + JSON.stringify(calls));

  // ⑤ 取消后回到新增态，保存走 POST
  ctx.openSubAdd();
  assert.strictEqual(els['subNameInput'].value, '', '新增态名称应清空');
  assert.strictEqual(els['subTitle'].textContent, '添加订阅', '标题应为添加订阅');
  assert.strictEqual(els['btnSubCreate'].textContent, '保存并导入', '按钮应为保存并导入');
  els['subUrlInput'].value = 'https://new.example/sub';
  await els['btnSubCreate'].onclick();
  const post = calls.filter(c => c.method === 'POST' && c.url.endsWith('/panel/api/subscriptions'));
  assert.strictEqual(post.length, 1, '新增应走 POST: ' + JSON.stringify(calls));
  console.log('proxy sub edit passed');
})().catch(e => { console.error('FAIL:', e && e.message); process.exit(1); });
`)
}
