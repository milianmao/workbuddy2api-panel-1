package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 导入时校验 refresh token（选项 A：RT 与 AT 都已死的新号直接拒收）。
//
// 背景：此前只有「覆盖导入」会先用 refresh 求证，新号是直接落盘的——等于把
// 「RT 早已失效、只等 AT 过期」的号静默塞进池子，它会在某天忽然 401，人只知道
// 要重新登录，说不出是哪一批导出带进来的。这里锁死三条：
//  1. 新号也必须真的发一次 refresh（否则谎报「已校验」）；
//  2. dead + AT 也过期 → 拒收：不入池、不落盘、errors 里写明需重新登录；
//  3. dead 但 AT 仍有效 → 收（issue #83：refresh 判死而 chat 仍 200），单列进 rt_dead；
//  4. 无判词（5xx/超时）→ 照收，单列进 unverified，不能因为网络抖动丢号。

// importRTEnv 空池面板：所有导入都是「新号」，refresh 端点走 refresh()。
func importRTEnv(t *testing.T, refresh func() (int, string), calls *int32) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc2(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusNotFound, `{}`
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				if calls != nil {
					atomic.AddInt32(calls, 1)
				}
				status, body = refresh()
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", BillingBaseCN: "https://cn.example",
	}
	pl := pool.New("")
	return New(Config{Pool: pl, Upstream: up, AuthDir: dir}), pl, dir
}

// importRTPost 用给定账号列表做一次真实（非预览）导入，返回解出来的响应。
func importRTPost(t *testing.T, pn *Panel, list []map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body)
	}
	return got
}

func importRTInt(t *testing.T, resp map[string]any, key string) int {
	t.Helper()
	v, _ := resp[key].(float64)
	return int(v)
}

func importRTList(t *testing.T, resp map[string]any, key string) []any {
	t.Helper()
	l, _ := resp[key].([]any)
	return l
}

// TestImportCockpitVerifiesNewAccountRefresh 新号也要真的求证一次：refreshCalls 必须是 1。
// 变异判据：把请求循环里的 `toVerify = append(...)` 退回「新号直接 commit」，本用例立刻红
// （refreshCalls=0）。
func TestImportCockpitVerifiesNewAccountRefresh(t *testing.T) {
	var calls int32
	pn, pl, dir := importRTEnv(t, func() (int, string) { return http.StatusOK, importRefreshOK }, &calls)
	resp := importRTPost(t, pn, []map[string]any{{
		"uid": "rt-ok", "access_token": "at", "refresh_token": "rt",
		"expires_at": time.Now().Add(24 * time.Hour).UnixMilli(),
	}})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("新号导入必须求证一次 refresh，实际 refresh 调用 %d 次", got)
	}
	if got := importRTInt(t, resp, "imported"); got != 1 {
		t.Fatalf("imported=%d want 1 (%v)", got, resp)
	}
	if l := importRTList(t, resp, "rt_dead"); len(l) != 0 {
		t.Fatalf("求证通过不该进 rt_dead：%v", l)
	}
	if l := importRTList(t, resp, "unverified"); len(l) != 0 {
		t.Fatalf("求证通过不该进 unverified：%v", l)
	}
	if st, ok := pl.Status("rt-ok"); !ok || !st.Hold {
		t.Fatalf("新号应入池且 hold，got ok=%v st=%+v", ok, st)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-rt-ok.json")); err != nil {
		t.Fatalf("求证通过应落盘：%v", err)
	}
}

// TestImportCockpitRejectsDeadRefreshWithExpiredAT RT 与 AT 都已死：这个号既不能用也不能续，
// 收进来只是一条要人伺候的死号 → 拒收。
func TestImportCockpitRejectsDeadRefreshWithExpiredAT(t *testing.T) {
	pn, pl, dir := importRTEnv(t, func() (int, string) {
		return http.StatusBadRequest, `{"code":12153,"msg":"Invalid refresh token"}`
	}, nil)
	resp := importRTPost(t, pn, []map[string]any{{
		"uid": "rt-dead", "access_token": "at", "refresh_token": "rt",
		"expires_at": time.Now().Add(-2 * time.Hour).UnixMilli(),
	}})

	if got := importRTInt(t, resp, "skipped"); got != 1 {
		t.Fatalf("skipped=%d want 1 (%v)", got, resp)
	}
	if got := importRTInt(t, resp, "imported"); got != 0 {
		t.Fatalf("imported=%d want 0 (%v)", got, resp)
	}
	errs, _ := resp["errors"].([]any)
	if len(errs) != 1 || !strings.Contains(errs[0].(string), "需重新登录") {
		t.Fatalf("errors 应写明需重新登录：%v", errs)
	}
	if _, ok := pl.Status("rt-dead"); ok {
		t.Fatal("死号不该入池")
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-rt-dead.json")); !os.IsNotExist(err) {
		t.Fatalf("死号不该落盘：%v", err)
	}
}

// TestImportCockpitKeepsDeadRefreshWithAliveAT RT 死了但 AT 还有 5 天：此刻 chat 仍正常
// （issue #83 实测 4/29 个号 refresh 恒回 12153，chat 全 200），丢号就是误杀。
// 收进来，但必须进 rt_dead 并带上到期天数——那是重登死线。
func TestImportCockpitKeepsDeadRefreshWithAliveAT(t *testing.T) {
	pn, pl, _ := importRTEnv(t, func() (int, string) {
		return http.StatusBadRequest, `{"code":12153,"msg":"Invalid refresh token"}`
	}, nil)
	resp := importRTPost(t, pn, []map[string]any{{
		"uid": "rt-half", "access_token": "at", "refresh_token": "rt",
		"expires_at": time.Now().Add(5 * 24 * time.Hour).UnixMilli(),
	}})

	if got := importRTInt(t, resp, "imported"); got != 1 {
		t.Fatalf("imported=%d want 1 (%v)", got, resp)
	}
	l := importRTList(t, resp, "rt_dead")
	if len(l) != 1 {
		t.Fatalf("rt_dead 应有 1 条：%v", resp)
	}
	note, _ := l[0].(map[string]any)
	if note["uid"] != "rt-half" {
		t.Fatalf("rt_dead.uid=%v", note["uid"])
	}
	days, _ := note["days_left"].(float64)
	if int(days) < 4 || int(days) > 5 {
		t.Fatalf("days_left=%v want 4~5（5 天后到期）", note["days_left"])
	}
	if st, ok := pl.Status("rt-half"); !ok || !st.Paused || !st.Hold {
		t.Fatalf("应收进池子且 paused+hold，got ok=%v st=%+v", ok, st)
	}
}

// TestImportCockpitImportsNewAccountUnverifiedOnInconclusive 上游 5xx：没有判词。
// 没有判词就不能丢号，照原样入池，但记进 unverified。
func TestImportCockpitImportsNewAccountUnverifiedOnInconclusive(t *testing.T) {
	pn, pl, _ := importRTEnv(t, func() (int, string) {
		return http.StatusBadGateway, `{"code":0,"msg":"bad gateway"}`
	}, nil)
	resp := importRTPost(t, pn, []map[string]any{{
		"uid": "rt-unknown", "access_token": "at", "refresh_token": "rt",
		"expires_at": time.Now().Add(24 * time.Hour).UnixMilli(),
	}})

	if got := importRTInt(t, resp, "imported"); got != 1 {
		t.Fatalf("imported=%d want 1 (%v)", got, resp)
	}
	l := importRTList(t, resp, "unverified")
	if len(l) != 1 || l[0] != "rt-unknown" {
		t.Fatalf("unverified=%v want [rt-unknown]", l)
	}
	if _, ok := pl.Status("rt-unknown"); !ok {
		t.Fatal("无判词也应入池")
	}
	if l := importRTList(t, resp, "rt_dead"); len(l) != 0 {
		t.Fatalf("5xx 不是死号判词，不该进 rt_dead：%v", l)
	}
}

// TestImportCockpitDedupesRefreshOnDuplicateUID 同一批里重复的 uid 在本地就被去重，
// 只有第一条会发 refresh（否则第二条会把第一条刚轮换到的令牌再烧一次）。
func TestImportCockpitDedupesRefreshOnDuplicateUID(t *testing.T) {
	var calls int32
	pn, _, _ := importRTEnv(t, func() (int, string) { return http.StatusOK, importRefreshOK }, &calls)
	resp := importRTPost(t, pn, []map[string]any{
		{"uid": "rt-dup", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().Add(time.Hour).UnixMilli()},
		{"uid": "rt-dup", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().Add(time.Hour).UnixMilli()},
	})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("重复 uid 只该求证一次，实际 %d", got)
	}
	if got := importRTInt(t, resp, "imported"); got != 1 {
		t.Fatalf("imported=%d want 1 (%v)", got, resp)
	}
	if got := importRTInt(t, resp, "skipped"); got != 1 {
		t.Fatalf("skipped=%d want 1 (%v)", got, resp)
	}
}
