package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/loginsite"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestOAuthLoginParksAccountPaused 端到端证明「OAuth 登录进来的号当场暂停选号」：
// 打桩上游设备授权三端点（auth/state、auth/token、login/account），走完整的
// POST /panel/api/login/start → GET /panel/api/login/poll 链路，然后断言
//
//  1. 凭证已落盘、号已入池；
//  2. 入池即 paused + hold（面板显示「新号待确认」），Pick() 选不到它；
//  3. 解冻（Revive）也放不出来——必须人工「恢复选号」（Resume）才进选号池。
//
// 为什么值得一测：登录路径曾经只做 Add+Revive，新号落池即接单；实测有登录后第一个
// 请求就 403 code=11140 的封号，错误直接打到线上流量。此处把该口径钉死。
func TestOAuthLoginParksAccountPaused(t *testing.T) {
	const uid = "uid-oauth-parked"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st-1","authUrl":"https://example.invalid/auth?state=st-1"}}`))
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-oauth","refreshToken":"rt-oauth","expiresIn":3600,"domain":"www.codebuddy.cn"}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"` + uid + `","nickname":"OAuth新号"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 站点解析指向 httptest（复用上游 login_site_test.go 的 withTestSite，避免打真实上游）。
	withTestSite(t, srv.URL, loginsite.SiteCN)

	dir := t.TempDir()
	pl := pool.New("")
	pn := New(Config{Version: "test", Pool: pl, AuthDir: dir})

	// 1) 发起授权
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login/start code=%d body=%s", rec.Code, rec.Body)
	}
	var start struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.State == "" {
		t.Fatalf("login/start 未返回 state: %s", rec.Body)
	}

	// 2) 轮询到登录完成
	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state="+start.State, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login/poll code=%d body=%s", rec.Code, rec.Body)
	}
	var done struct {
		Done bool   `json:"done"`
		UID  string `json:"uid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatal(err)
	}
	if !done.Done || done.UID != uid {
		t.Fatalf("登录未完成: %s", rec.Body)
	}

	// 3) 凭证落盘 + 入池
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-"+uid+".json")); err != nil {
		t.Fatalf("凭证未落盘: %v", err)
	}
	if pl.AuthByUID(uid) == nil {
		t.Fatal("登录后账号应已入池（保号任务照常跑）")
	}

	// 4) 核心断言：入池即暂停 + 待确认，不进选号池
	st, ok := pl.Status(uid)
	if !ok || !st.Paused || !st.Hold {
		t.Fatalf("OAuth 新号应落为 paused+hold，实际 %+v ok=%v", st, ok)
	}
	if got := pl.Pick(); got != nil {
		t.Fatalf("OAuth 新号不该被选中，实际 uid=%s", got.UID)
	}

	// 5) 解冻（Revive）也放不出来
	pl.Revive(uid)
	if got := pl.Pick(); got != nil {
		t.Fatalf("解冻后仍不得进选号池，实际 uid=%s", got.UID)
	}

	// 6) 人工恢复选号才进池
	if !pl.Resume(uid) {
		t.Fatal("Resume 应成功")
	}
	if got := pl.Pick(); got == nil || got.UID != uid {
		t.Fatalf("恢复选号后应可选，实际 %+v", got)
	}
}
