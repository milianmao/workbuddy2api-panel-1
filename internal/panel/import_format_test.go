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
		// 2026-11-09 现场报错的那一类：信封的值是**对象**而不是数组
		// （`invalid json: cannot unmarshal object into []panel.cockpitAccount`）
		{"信封里是单账号对象", `{"accounts":` + accJSON + `}`, 1, "uid-1"},
		{"data 里是单账号 auth 文件", `{"data":` + authFileJSON + `}`, 1, "777bd8bc"},
		{"顶层对象但对不上", `{"meta":{"version":1},"exported":true}`, -1, ""},
		{"不是 JSON", `uid,access_token`, -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeCockpitAccounts([]byte(c.in))
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
	got, err := decodeCockpitAccounts([]byte(authFileJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("账号数 = %d，期望 1", len(got))
	}
	acc := got[0]
	if acc.AccessToken != "eyJat" || acc.RefreshToken != "eyJrt" {
		t.Fatalf("token 解析错: %q / %q", acc.AccessToken, acc.RefreshToken)
	}
	if acc.Realm != "cn" {
		t.Fatalf("realm 应保留 cn，得到 %q", acc.Realm)
	}
	// auth 文件写的是秒；normalizeExpiresAtMillis 必须把它放大成毫秒，再 /1000 还是原值。
	if sec := normalizeExpiresAtMillis(acc.ExpiresAt) / 1000; sec != 1795096802 {
		t.Fatalf("秒级 expiresAt 不该被当成毫秒再除：%d", sec)
	}

	// cockpit 导出的毫秒 expires_at 必须归一成秒，否则 ExpiresAt 会变成 6 万年后的未来。
	ck, err := decodeCockpitAccounts([]byte(accJSON))
	if err != nil {
		t.Fatal(err)
	}
	if sec := normalizeExpiresAtMillis(ck[0].ExpiresAt) / 1000; sec != 1893456000 {
		t.Fatalf("毫秒 expires_at 应归一成秒 1893456000，得到 %d", sec)
	}
}

// device_token 是设备指纹基座，auth 文件带过来时必须跟着走。
func TestImportKeepsDeviceToken(t *testing.T) {
	in := `{"account":{"uid":"u-9","nickname":"n"},"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1795096802,"domain":"www.codebuddy.cn","realm":"global","device_token":"dt-1"}}`
	got, err := decodeCockpitAccounts([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DeviceToken != "dt-1" {
		t.Fatalf("device_token 丢了: %+v", got)
	}
	if got[0].Realm != "global" {
		t.Fatalf("realm 应为 global，得到 %q", got[0].Realm)
	}
}

// 缺字段时必须点名缺的是哪个（面板弹窗就显示这一句），不能再只报「失败」。
func TestImportSkipReasonNamesMissingField(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), AuthDir: t.TempDir()})
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
	_, err := decodeCockpitAccounts([]byte(`{"meta":{"version":1},"exported":true}`))
	if err == nil {
		t.Fatal("对不上的对象应报错")
	}
	for _, want := range []string{"无法识别", "账号数组"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际: %v", want, err)
		}
	}
}

func TestImportCockpitHandlerRejectsUnparseableObject(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), AuthDir: t.TempDir()})
	code, got := postImport(t, p, `{"meta":{"version":1},"exported":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", code)
	}
	if strings.Contains(got, "cannot unmarshal") {
		t.Fatalf("不该再回 Go 类型错误: %s", got)
	}
	if !strings.Contains(got, "无法识别") {
		t.Fatalf("应给出可排查的中文提示: %s", got)
	}
}
