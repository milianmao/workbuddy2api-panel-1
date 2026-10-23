package panel

// 导入外壳兼容性测试。
//
// 2026-10-23 两轮现场：先是「invalid json: json: cannot unmarshal object into Go value
// of type []panel.cockpitAccount」（只认顶层数组），修完变成「导入完成但 0 个成功」——
// 因为用户手上的是**面板/插件 auth 文件**形状（嵌套 {"account":{uid},"auth":{accessToken}}、
// camelCase），而解析只按 cockpit 的 snake_case 字段读，uid/at/rt 全空被当缺字段跳过。
// 所以这里既测外壳，也测「auth 文件形状」这条真实来源。

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// cockpit 导出的 snake_case 一行（expires_at 为毫秒）。
const accJSON = `{"uid":"uid-1","email":"a@b.c","access_token":"at","refresh_token":"rt","expires_at":1893456000000,"domain":"workbuddy.cn"}`

// 面板/插件 auth 文件形状（嵌套 + camelCase，expiresAt 为秒）。
const authFileJSON = `{"account":{"enterpriseId":"","nickname":"135****9374","uid":"777bd8bc"},"auth":{"accessToken":"eyJat","refreshToken":"eyJrt","expiresAt":1795096802,"domain":"www.codebuddy.cn","realm":"cn"}}`

func TestParseCockpitAccountsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // 期望解析出的账号数；-1 表示期望报错
		uid  string
	}{
		{"裸数组", `[` + accJSON + `]`, 1, "uid-1"},
		{"对象包 accounts", `{"accounts":[` + accJSON + `],"exported_at":1700000000}`, 1, "uid-1"},
		{"对象包 data 数组", `{"data":[` + accJSON + `]}`, 1, "uid-1"},
		{"对象套对象", `{"version":2,"data":{"accounts":[` + accJSON + `]}}`, 1, "uid-1"},
		{"字段名大小写不敏感", `{"Accounts":[` + accJSON + `]}`, 1, "uid-1"},
		{"data 里放 list", `{"data":{"list":[` + accJSON + `]}}`, 1, "uid-1"},
		{"单账号对象", accJSON, 1, "uid-1"},
		{"二次编码成字符串", `"[` + strings.ReplaceAll(accJSON, `"`, `\"`) + `]"`, 1, "uid-1"},
		{"带 BOM", "\xef\xbb\xbf[" + accJSON + "]", 1, "uid-1"},
		// 真实来源：auths/ 目录里的那种文件。
		{"auth 文件单对象", authFileJSON, 1, "777bd8bc"},
		{"auth 文件在数组里", `[` + authFileJSON + `]`, 1, "777bd8bc"},
		{"auth 文件在 accounts 里", `{"accounts":[` + authFileJSON + `]}`, 1, "777bd8bc"},
		{"扁平 camelCase", `{"uid":"u-2","accessToken":"at","refreshToken":"rt","expiresAt":1795096802}`, 1, "u-2"},
		{"以 uid 为键的字典", `{"k1":` + accJSON + `,"k2":` + authFileJSON + `}`, 2, "777bd8bc"},
		{"外壳里是空数组", `{"accounts":[]}`, 0, ""},
		{"顶层对象但对不上", `{"meta":{"version":1},"exported":true}`, -1, ""},
		{"不是 JSON", `uid,access_token`, -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCockpitAccounts([]byte(c.in))
			if c.want < 0 {
				if err == nil {
					t.Fatalf("期望报错，实际解析出 %d 个账号", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(got) != c.want {
				t.Fatalf("账号数 = %d，期望 %d", len(got), c.want)
			}
			if c.want > 0 {
				found := false
				for _, a := range got {
					if a.UID == c.uid {
						found = true
					}
				}
				if !found {
					t.Fatalf("没解析出 uid=%q", c.uid)
				}
			}
		})
	}
}

// auth 文件形状带过来的 realm / 企业 id / 过期时间不能被丢掉：
// 企业号当个人号跑成长任务会被上游一路 400，过期时间单位错了则会立刻触发 refresh。
func TestDecodeCockpitAccountKeepsAuthFileFields(t *testing.T) {
	acc, err := decodeCockpitAccount([]byte(authFileJSON))
	if err != nil {
		t.Fatal(err)
	}
	if acc.AccessToken != "eyJat" || acc.RefreshToken != "eyJrt" {
		t.Fatalf("token 解析错: %q / %q", acc.AccessToken, acc.RefreshToken)
	}
	if acc.Realm != "cn" {
		t.Fatalf("realm 应保留 cn，得到 %q", acc.Realm)
	}
	if acc.expiresAtSec != 1795096802 {
		t.Fatalf("秒级 expiresAt 不该被当成毫秒再除：%d", acc.expiresAtSec)
	}

	// cockpit 导出的毫秒 expires_at 必须归一成秒，否则 ExpiresAt 会变成 6 万年后的未来。
	ck, err := decodeCockpitAccount([]byte(accJSON))
	if err != nil {
		t.Fatal(err)
	}
	if ck.expiresAtSec != 1893456000 {
		t.Fatalf("毫秒 expires_at 应归一成秒 1893456000，得到 %d", ck.expiresAtSec)
	}
}

// 缺字段时必须点名缺的是哪个（面板弹窗就显示这一句），不能再只报「失败」。
func TestImportSkipReasonNamesMissingField(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	// 只有 accessToken，没有 refreshToken / uid。
	body := `[{"access_token":"at","domain":"www.codebuddy.cn"}]`
	code, out := postImport(t, p, body)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（解析成功但跳过）", code)
	}
	if !strings.Contains(out, "缺字段") || !strings.Contains(out, "refreshToken") || !strings.Contains(out, "uid") {
		t.Fatalf("跳过原因应点名缺 uid/refreshToken: %s", out)
	}
}

func postImport(t *testing.T, p *Panel, payload string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "cockpit.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)
	return rec.Code, rec.Body.String()
}

func TestParseCockpitAccountErrorIsActionable(t *testing.T) {
	_, err := parseCockpitAccounts([]byte(`{"meta":{"version":1},"exported":true}`))
	if err == nil {
		t.Fatal("对不上的对象应报错")
	}
	for _, want := range []string{"meta", "exported", "顶层"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际: %v", want, err)
		}
	}
}

func TestImportCockpitHandlerRejectsUnparseableObject(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	code, got := postImport(t, p, `{"meta":{"version":1},"exported":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", code)
	}
	if strings.Contains(got, "cannot unmarshal") {
		t.Fatalf("不该再回 Go 类型错误: %s", got)
	}
	if !strings.Contains(got, "顶层") {
		t.Fatalf("应给出可排查的中文提示: %s", got)
	}
}

// 导入弹窗一次可选多个文件：input 必须带 multiple，处理函数必须逐个上传并汇总，
// 单个文件失败不能连累整批（容器内无 node 会 skip，在有 node 的机器上生效）。
func TestImportUIAllowsMultipleFiles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; import multi-file ui test skipped")
	}
	path := filepath.Join(t.TempDir(), "importmulti.cjs")
	if err := os.WriteFile(path, []byte(importMultiScript), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("import multi-file ui test failed: %v\n%s", err, out)
	}
}

const importMultiScript = `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');

// 1) 导入框必须支持多选
const tag = /<input[^>]*id="importFile"[^>]*>/.exec(html);
assert.ok(tag, 'index.html 缺少 #importFile');
assert.ok(/\bmultiple\b/.test(tag[0]), 'importFile 必须带 multiple: ' + tag[0]);
assert.ok(/一次多选/.test(html), '提示文案应告诉用户可以多选');

// 2) 切出 onchange 处理函数（不是整份 app.js：它启动时会摸一堆 DOM）
const start = src.indexOf("$('importFile').onchange");
const end = src.indexOf('/* ── 顶部动作');
assert.ok(start >= 0 && end > start, '找不到 importFile.onchange');

const els = {
  importFile: { files: [{ name: 'a.json' }, { name: 'b.json' }, { name: 'c.json' }], value: 'picked' },
  importDone: { hidden: true, textContent: '' },
  importErr: { hidden: true, textContent: '' },
};
const uploads = [];
let overviewReloaded = 0;
const ctx = {
  console,
  LS_KEY: 'k',
  localStorage: { getItem: () => 'test-key' },
  FormData: class { constructor() { this.items = []; } append(k, v) { this.items.push([k, v]); } },
  fetch: async (url, opt) => {
    const fd = opt.body;
    assert.strictEqual(fd.items.length, 1, '每个请求只该带一个 file 字段');
    const name = fd.items[0][1].name;
    uploads.push([url, name]);
    if (name === 'b.json') {
      return { ok: false, status: 400, json: async () => ({ error: 'invalid json: 顶层是对象但里面没找到账号数组' }) };
    }
    return {
      ok: true, status: 200,
      json: async () => name === 'c.json'
        ? { ok: true, imported: 2, skipped: 1, errors: ['missing required fields (id=9)'] }
        : { ok: true, imported: 2, skipped: 0, errors: [] },
    };
  },
  invalidatePackages: () => {},
  loadOverview: () => { overviewReloaded++; },
  $: id => els[id],
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
assert.strictEqual(typeof els.importFile.onchange, 'function', 'onchange 未挂上');

els.importFile.onchange().then(() => {
  // 3) 三个文件逐个上传（不是只取 files[0]）
  assert.deepStrictEqual(uploads.map(u => u[1]), ['a.json', 'b.json', 'c.json'],
    '应按顺序逐个上传，实际: ' + JSON.stringify(uploads));
  assert.ok(uploads.every(u => u[0] === '/panel/api/import/cockpit'), '端点应一致');

  // 4) 汇总：a+c 成功各 2 个号 → 4 个号；c 里 1 个号被跳过；b 整个文件失败
  const done = els.importDone.textContent;
  assert.ok(els.importDone.hidden === false, '完成提示应显示');
  assert.ok(/3 个文件/.test(done), '应报文件数: ' + done);
  assert.ok(/成功 4 个号/.test(done), '应汇总成功账号数: ' + done);
  assert.ok(/跳过 1 个号/.test(done), '应汇总跳过账号数: ' + done);
  assert.ok(/1 个文件失败/.test(done), '应报失败文件数: ' + done);

  // 5) 失败文件的原因要露出来（含具体是哪个文件），不能只吞进控制台
  assert.ok(els.importErr.hidden === false, '失败详情应显示');
  const err = els.importErr.textContent;
  assert.ok(/b\.json/.test(err), '错误里应带失败文件名: ' + err);
  assert.ok(/顶层是对象/.test(err), '错误里应带服务端原因: ' + err);
  assert.ok(/c\.json/.test(err), '被跳过的账号原因也应带文件名: ' + err);

  // 6) 收尾：清空 input 以便重复导入同一批文件（不清理则选同一文件不触发 onchange）
  assert.strictEqual(els.importFile.value, '', '导入后应清空 input');
  assert.strictEqual(overviewReloaded, 1, '整批结束后只刷新一次列表');
  console.log('import multi-file ui passed');
}).catch(e => { console.error(e); process.exit(1); });
`
