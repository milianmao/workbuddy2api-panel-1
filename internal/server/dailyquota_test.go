package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestDailyQuotaAPI 验证真实上游扣费只累计一次，聊天与图片达额后均返回可辨认的 429。
func TestDailyQuotaAPI(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(boolLit(stream), func(t *testing.T) {
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return 200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":2,\"credit\":0.6}}\n\ndata: [DONE]\n\n", true
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			defer p.Close()
			p.SetDailyCreditLimits(0.5, nil)
			up.AccountQuotaCheck, up.AccountCreditObserver = p.CheckDailyQuota, p.RecordDailyCredit
			h := NewHandler(Config{Pool: p, Upstream: up})
			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":` + boolLit(stream) + `}`
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if w.Code != 200 {
				t.Fatalf("first request=%d %s", w.Code, w.Body)
			}
			st, _ := p.Status("u1")
			if st.DailyQuota.Used != 0.6 {
				t.Fatalf("重复或遗漏扣费：%+v", st.DailyQuota)
			}
			for _, path := range []string{"/v1/chat/completions", "/v1/images/generations", "/v1/images/edits"} {
				w = httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"glm-5.2","prompt":"cat","image":["data:image/png;base64,YQ=="],"messages":[{"role":"user","content":"hi"}]}`)))
				if w.Code != 429 || !strings.Contains(w.Body.String(), "account_daily_limit_exceeded") || w.Header().Get("Retry-After") == "" {
					t.Fatalf("%s status=%d headers=%v body=%s", path, w.Code, w.Header(), w.Body)
				}
			}
			if calls != 1 {
				t.Fatalf("达额后继续调用上游：calls=%d", calls)
			}
		})
	}
}

// TestDailyQuotaContinuationAndSearch 验证续写逐段结算后再准入，最终聚合用量不会重复计入。
func TestDailyQuotaContinuationAndSearch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     float64
		wantCalls int
		wantUsed  float64
	}{{"stop_after_first_segment", 0.3, 1, 0.4}, {"count_each_segment_once", 0.8, 2, 0.8}} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			defer p.Close()
			p.SetDailyCreditLimits(tc.limit, nil)
			up, requests := fakeContinueUpstream(t, func(call int) (int, string) {
				if call == 1 {
					return 200, strings.Replace(testSeg1Text, `"total_tokens":15`, `"total_tokens":15,"credit":0.4`, 1)
				}
				return 200, strings.Replace(testSeg2Text, `"total_tokens":27`, `"total_tokens":27,"credit":0.4`, 1)
			})
			up.AccountQuotaCheck, up.AccountCreditObserver = p.CheckDailyQuota, p.RecordDailyCredit
			h := NewHandler(Config{Pool: p, Upstream: up})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
			st, _ := p.Status("u1")
			if w.Code != 200 || len(*requests) != tc.wantCalls || st.DailyQuota.Used != tc.wantUsed {
				t.Fatalf("续写 status=%d calls=%d quota=%+v body=%s", w.Code, len(*requests), st.DailyQuota, w.Body)
			}
			// 搜索探测走独立调用链，但同样不能重新选到达额账号。
			if _, err := h.probeOnce(context.Background(), "cn", "glm-5.2", []byte(`{"model":"glm-5.2"}`)); err == nil || !strings.Contains(err.Error(), "每日限额") {
				t.Fatalf("search probe quota error=%v", err)
			}
			if len(*requests) != tc.wantCalls {
				t.Fatal("达额后搜索探测仍然出站")
			}
		})
	}
}

// TestDailyQuotaAdmissionRace 不得把选号之后的本地额度拒绝送入传输故障的连败惩罚。
func TestDailyQuotaAdmissionRace(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	defer p.Close()
	p.SetDailyCreditLimits(1, nil)
	p.SetDegrade(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Fatal("限额竞态后仍然出站")
		return 200, "", true
	})
	up.AccountQuotaCheck = func(uid string) error {
		p.RecordDailyCredit(uid, 1, true)
		return p.CheckDailyQuota(uid)
	}
	h := NewHandler(Config{Pool: p, Upstream: up})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != 429 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	p.SetDailyCreditLimits(0, nil)
	if p.Pick() == nil {
		t.Fatal("本地限额拒绝误触发了账号降权/冷却")
	}
}

// TestDailyQuotaAdapters 验证 Responses 和 Messages 转换链保留账号日限额的状态码与原因。
func TestDailyQuotaAdapters(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	defer p.Close()
	p.SetDailyCreditLimits(1, nil)
	p.RecordDailyCredit("u1", 1, true)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Fatal("达额后适配入口仍然出站")
		return 200, "", true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"m","input":"hi"}`},
		{"/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		if w.Code != 429 || !strings.Contains(w.Body.String(), "account_daily_limit_exceeded") {
			t.Fatalf("%s status=%d body=%s", tc.path, w.Code, w.Body)
		}
	}
}
