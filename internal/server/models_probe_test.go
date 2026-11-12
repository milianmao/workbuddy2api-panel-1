// models_probe_test.go 钉住「模型目录探针」的两条兜底：
//
//  1. 第一个候选号拉不到目录（出口坏了）时，必须换下一个号，而不是把整份 CN 目录丢掉；
//  2. 所有候选都失败时，必须继续给上一次成功的目录（stale-while-error），不要把
//     /v1/models 从 22 条缩成出图/出视频那 4 条。
//
// 背景（2026-11-12 实案）：目录探针原先只取 AvailableUIDsForRealm("cn")[0]，那个号
// 钉在一台死节点上，于是 CR 目录整域拉不到；面板模型页只剩 4 条、/v1/models 同步缩水，
// 还进 5 分钟负缓存，看上去像「模型没了」。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const probeCatalogBody = `{"code":0,"data":{"models":[` +
	`{"id":"deepseek-v4.1-flash","maxInputTokens":1000,"maxOutputTokens":8192},` +
	`{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000}],` +
	`"agents":[{"name":"cli","models":["deepseek-v4.1-flash","glm-5.2"]}]}}`

// resetModelsProbeCache 清全局目录缓存，避免用例之间互相污染（复用 hidden_models_test.go
// 里既有的 resetDynamicModelsCache，别在本文件重名声明）。
func resetModelsProbeCache(t *testing.T) {
	t.Helper()
	resetDynamicModelsCache()
	t.Cleanup(resetDynamicModelsCache)
}

// modelsProbePool 造一个 cn 池：u1 绑死一个节点（令牌 bad → 上游 500），u2 直连正常。
// 字典序 u1 在前，正好复现「第一个号坏了」的现场。
func modelsProbePool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "bad", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "good", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetAccountProxy("u1", "node-dead")
	return p
}

// TestModelsProbeSkipsDeadEgressAccount 第一个候选号（绑在坏出口上）失败后要换号成功。
func TestModelsProbeSkipsDeadEgressAccount(t *testing.T) {
	resetModelsProbeCache(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "bad") {
			return http.StatusInternalServerError, `{"code":11134,"msg":"busy"}`, false
		}
		return http.StatusOK, probeCatalogBody, false
	})
	h := NewHandler(Config{Pool: modelsProbePool(t), Upstream: up})

	got := h.fetchDynamicModels()
	if len(got) == 0 {
		t.Fatalf("换号兜底没生效：目录为空（第一个号失败后应换下一个）")
	}
	var ids []string
	for _, mi := range got {
		ids = append(ids, mi.ID)
	}
	if strings.Join(ids, ",") != "deepseek-v4.1-flash,glm-5.2" {
		t.Fatalf("目录=%v want [deepseek-v4.1-flash glm-5.2]", ids)
	}
}

// TestModelsEndpointServesStaleOnTotalFailure 全部候选失败时沿用上一次成功的目录，
// 且负缓存期内也必须继续给（否则第一次降级后就再也取不到，直到冷却结束）。
func TestModelsEndpointServesStaleOnTotalFailure(t *testing.T) {
	resetModelsProbeCache(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusInternalServerError, `{"code":11134,"msg":"busy"}`, false
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "bad", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 预置一次成功目录，但已过期（模拟「上次拉到了、这次拉不到」）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "deepseek-v4.1-flash", MaxTokens: 8192}}
	dynamicModelsCache.fetched = time.Now().Add(-2 * dynamicModelsTTL)
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	for i := 0; i < 2; i++ { // 第二次要命中负缓存分支，同样得给 stale
		got := h.fetchDynamicModels()
		if len(got) != 1 || got[0].ID != "deepseek-v4.1-flash" {
			t.Fatalf("第 %d 次应沿用上次成功目录，实得 %v", i+1, got)
		}
	}

	rec := httptest.NewRecorder()
	h.models(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	found := false
	for _, d := range resp.Data {
		if d.ID == "cn:deepseek-v4.1-flash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/v1/models 未列出 stale 目录: %s", rec.Body.String())
	}
}
