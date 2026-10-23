package panel

// 导入外壳兼容性测试：面板报「导入失败：invalid json: json: cannot unmarshal object
// into Go value of type []panel.cockpitAccount」时，说明手上的导出文件顶层是对象而
// 不是数组。解析器现在要能吃下 cockpit tools 各版本的常见外壳，并且在对不上时给出
// 能照着排查的错误（回显顶层字段名 + 文件开头），而不是一句 Go 类型错误。

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

const accJSON = `{"uid":"uid-1","email":"a@b.c","access_token":"at","refresh_token":"rt","expires_at":1893456000000,"domain":"workbuddy.cn"}`

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
		{"以 uid 为键的字典", `{"k1":` + accJSON + `,"k2":{"uid":"uid-2","access_token":"at2","refresh_token":"rt2"}}`, 2, "uid-1"},
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
			if c.want > 0 && got[0].UID != c.uid {
				t.Fatalf("uid = %q，期望 %q", got[0].UID, c.uid)
			}
		})
	}
}

func TestParseCockpitAccountErrorIsActionable(t *testing.T) {
	_, err := parseCockpitAccounts([]byte(`{"meta":{"version":1},"exported":true}`))
	if err == nil {
		t.Fatal("对不上的对象应报错")
	}
	// 关键：错误里要有顶层字段名（meta/exported），否则用户看不出是哪层套错了。
	for _, want := range []string{"meta", "exported", "顶层"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际: %v", want, err)
		}
	}
}

// 面板层面的回归：这类文件以前返回 400 且文案是 Go 类型错误；现在要么解析成功，
// 要么返回能照着排查的中文错误。
func TestImportCockpitHandlerRejectsUnparseableObject(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "cockpit.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(`{"meta":{"version":1},"exported":true}`)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	got := rec.Body.String()
	if strings.Contains(got, "cannot unmarshal") {
		t.Fatalf("不该再回 Go 类型错误: %s", got)
	}
	if !strings.Contains(got, "顶层") {
		t.Fatalf("应给出可排查的中文提示: %s", got)
	}
}
