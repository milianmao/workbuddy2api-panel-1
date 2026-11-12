package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 面板模型页：CN 目录取数全部失败时，必须回落到上一次成功的目录并标 stale，
// 而不是把整屏缩成出图/出视频那几条（2026-11-12 实案：探针号钉在死节点上，
// 面板只剩 4 条、用户以为模型全没了）。
func TestModelsFallsBackToStaleCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":11134,"msg":"busy"}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.HTTP, up.ChatBaseCN = srv.Client(), srv.URL

	p := New(Config{Pool: poolOfOne(t), Upstream: up})
	p.modelsMu.Lock()
	p.modelsCache = &modelsCacheEntry{
		models: []map[string]any{
			{"id": "cn:deepseek-v4.1-flash", "object": "model"},
			{"id": "cn:glm-5.2", "object": "model"},
		},
	}
	// 过期：本次会重新探测 → 上游 500 → 应当回落到这份目录。
	p.modelsCacheAt = time.Now().Add(-2 * modelsCacheTTL)
	p.modelsMu.Unlock()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK          bool             `json:"ok"`
		Stale       bool             `json:"stale"`
		Models      []map[string]any `json:"models"`
		FetchErrors []string         `json:"fetch_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !resp.Stale {
		t.Fatalf("stale 未标注: %s", rec.Body.String())
	}
	if len(resp.Models) != 2 {
		t.Fatalf("模型数=%d want 2（应沿用上次成功目录）: %s", len(resp.Models), rec.Body.String())
	}
	if len(resp.FetchErrors) == 0 || !strings.HasPrefix(resp.FetchErrors[0], "cn: ") {
		t.Fatalf("fetch_errors=%v want 以 cn: 开头", resp.FetchErrors)
	}
}
