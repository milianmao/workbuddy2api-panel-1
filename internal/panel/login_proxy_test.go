package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/loginsite"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestLoginWithProxy 通过真实 HTTP 代理入口走完整登录链路，防止只代理授权而轮询直连。
func TestLoginWithProxy(t *testing.T) {
	for _, site := range []string{loginsite.SiteCN, loginsite.SiteIntl, loginsite.SiteWorkBuddy} {
		t.Run(site, func(t *testing.T) {
			var paths []string
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !r.URL.IsAbs() || r.URL.Host != "login.invalid" {
					t.Errorf("未收到代理请求: %s", r.URL)
				}
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/plugin/auth/state":
					fmt.Fprint(w, `{"code":0,"data":{"state":"via-proxy","authUrl":"https://login.invalid/authorize?state=via-proxy"}}`)
				case "/v2/plugin/auth/token":
					fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`)
				case "/v2/plugin/login/account":
					fmt.Fprint(w, `{"code":0,"data":{"uid":"proxy-user","nickname":"代理登录"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer proxy.Close()
			withTestSite(t, "http://login.invalid", site)
			pn := New(Config{APIKey: "secret", Pool: pool.New(""), AuthDir: t.TempDir()})
			body := fmt.Sprintf(`{"site":%q,"proxy":%q}`, site, proxy.URL)
			req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(body))
			rec := httptest.NewRecorder()
			pn.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || len(paths) != 0 {
				t.Fatalf("鉴权失败不应访问上游: %d %s", rec.Code, rec.Body)
			}
			req = httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			rec = httptest.NewRecorder()
			pn.ServeHTTP(rec, req)
			var start struct{ URL, State, Site string }
			if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || rec.Code != http.StatusOK || start.State != "via-proxy" || start.Site != site || strings.Contains(start.URL, "version=") {
				t.Fatalf("授权请求未走指定代理: %d %s", rec.Code, rec.Body)
			}
			req = httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state="+start.State, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec = httptest.NewRecorder()
			pn.ServeHTTP(rec, req)
			var done struct{ Done bool }
			if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil || rec.Code != http.StatusOK || !done.Done || len(paths) != 3 || pn.cfg.Pool.AuthByUID("proxy-user") == nil {
				t.Fatalf("轮询未沿用代理完成入池: paths=%v code=%d body=%s", paths, rec.Code, rec.Body)
			}
		})
	}
}

// TestLoginPollExpiresSession 保证过期会话在轮询入口作废，不再继续使用原代理请求上游。
func TestLoginPollExpiresSession(t *testing.T) {
	pn := New(Config{})
	pn.logins["expired"] = loginSession{created: time.Now().Add(-loginTTL - time.Second), processing: true}
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=expired", nil))
	if rec.Code != http.StatusGone || len(pn.logins) != 0 {
		t.Fatalf("过期会话应清理并返回 410: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestLoginProxyHTTPS 验证正式上游使用的 HTTPS 链路确实通过代理建立 CONNECT 隧道。
func TestLoginProxyHTTPS(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"state":"https-proxy"}}`)
	}))
	defer up.Close()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != up.Listener.Addr().String() {
			http.Error(w, "隧道目标不符", http.StatusBadRequest)
			return
		}
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer dst.Close()
		src, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer src.Close()
		connects.Add(1)
		fmt.Fprint(src, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(dst, src) }()
		_, _ = io.Copy(src, dst)
	}))
	defer proxy.Close()
	client, err := loginProxyClient(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig
	data, _, err := doJSON(t.Context(), client, http.MethodPost, up.URL, nil, strings.NewReader("{}"), siteOrDefault(loginsite.SiteCN))
	if err != nil || !strings.Contains(string(data), "https-proxy") || connects.Load() != 1 {
		t.Fatalf("HTTPS 未经代理隧道返回授权信息: data=%s CONNECT=%d err=%v", data, connects.Load(), err)
	}
}

// TestLoginProxyFailureDoesNotDialDirect 防止指定代理不可用时泄露真实服务端出口。
func TestLoginProxyFailureDoesNotDialDirect(t *testing.T) {
	var direct atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		fmt.Fprint(w, `{"code":0,"data":{"state":"direct","authUrl":"https://example.invalid"}}`)
	}))
	defer up.Close()
	withTestSite(t, up.URL, loginsite.SiteCN)
	proxy := httptest.NewServer(http.NotFoundHandler())
	proxy.Close()
	pn := New(Config{})
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"site":"cn","proxy":%q}`, proxy.URL)
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(body)))
	if rec.Code != http.StatusBadGateway || direct.Load() != 0 || len(pn.logins) != 0 {
		t.Fatalf("代理失败不得直连或创建会话: code=%d direct=%d body=%s", rec.Code, direct.Load(), rec.Body)
	}
}

// TestLoginRejectsInvalidProxyInput 覆盖外部输入边界，错误参数不得回退为默认站点或直连。
func TestLoginRejectsInvalidProxyInput(t *testing.T) {
	for _, body := range []string{
		`{"site":"unknown"}`,
		`{"site":"intl","proxy":"localhost:7890"}`,
		`{"proxy":"ftp://user:password@localhost:21"}`,
		`{"proxy":"http://localhost:7890/path"}`,
		`{"site":`,
		`{} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			pn := New(Config{})
			rec := httptest.NewRecorder()
			pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest || len(pn.logins) != 0 || strings.Contains(rec.Body.String(), "password") {
				t.Fatalf("非法参数应返回 400 且不泄露代理密码: %d %s", rec.Code, rec.Body)
			}
		})
	}
}
