package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestAccountQuotaAllModelEntrypoints 验证后台专家、普通聊天和图片入口均在发 HTTP 前检查限额。
func TestAccountQuotaAllModelEntrypoints(t *testing.T) {
	blocked := errors.New("account daily limit")
	c := testClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("日限额拒绝后仍发出 HTTP 请求")
		return nil, nil
	})
	c.AccountQuotaCheck = func(string) error { return blocked }
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	_, _, _, err := c.ChatStream(a, []byte(`{"model":"m"}`), "", ChatMeta{})
	if !errors.Is(err, blocked) {
		t.Fatalf("chat guard: %v", err)
	}
	if _, _, err = c.DesktopChatWithExpert(a, "expert"); !errors.Is(err, blocked) {
		t.Fatalf("expert guard: %v", err)
	}
	if _, err = c.ImageGenerations(a, map[string]any{"model": "image"}); !errors.Is(err, blocked) {
		t.Fatalf("image generation guard: %v", err)
	}
	if _, err = c.ImageEdits(a, map[string]any{"model": "image"}); !errors.Is(err, blocked) {
		t.Fatalf("image edit guard: %v", err)
	}
	if _, err = c.Search(context.Background(), a, "query", 5); !errors.Is(err, blocked) {
		t.Fatalf("search guard: %v", err)
	}
}

// TestAccountQuotaStreamObservation 验证透明透传、重复回执取最终快照、Close 不重复计费，缺失不伪装免费。
func TestAccountQuotaStreamObservation(t *testing.T) {
	var credits []float64
	var observed []bool
	c := &Client{AccountCreditObserver: func(uid string, credit float64, has bool) {
		if uid != "u1" {
			t.Fatalf("observer uid=%q", uid)
		}
		credits, observed = append(credits, credit), append(observed, has)
	}}
	wire := "data: {\"usage\":{\"credit\":0.1}}\r\n\r\ndata: {\"usage\":{\"credit\":0.3}}\n\ndata: [DONE]\n\n"
	r := c.observeModelStream("u1", io.NopCloser(strings.NewReader(wire)))
	var got strings.Builder
	buf := make([]byte, 3) // 故意拆开 JSON 与换行，验证真实 Reader 分片边界。
	if _, err := io.CopyBuffer(&got, r, buf); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r.Close()
	if got.String() != wire || len(credits) != 1 || credits[0] != 0.3 || !observed[0] {
		t.Fatalf("wire preserved=%v credits=%v observed=%v", got.String() == wire, credits, observed)
	}
	r = c.observeModelStream("u1", io.NopCloser(strings.NewReader("data: {\"usage\":{\"credit\":0}}")))
	io.Copy(io.Discard, r)
	r.Close()
	r = c.observeModelStream("u1", io.NopCloser(strings.NewReader("data: [DONE]\n")))
	io.Copy(io.Discard, r)
	r.Close()
	if len(credits) != 3 || !observed[1] || credits[1] != 0 || observed[2] {
		t.Fatalf("免费和缺失被混淆：credits=%v observed=%v", credits, observed)
	}
}

// TestAccountQuotaExpertAndImageReceipts 验证专家任务不在首个 requestId 提前返回，图片扣费不要求 tokens。
func TestAccountQuotaExpertAndImageReceipts(t *testing.T) {
	const id = "cmb-0123456789abcdef0123456789abcdef"
	var credits []float64
	c := testClient(func(req *http.Request) (*http.Response, error) {
		wire := "data: {\"id\":\"" + id + "\"}\n\ndata: {\"usage\":{\"credit\":0.25}}\n\ndata: [DONE]\n\n"
		if strings.Contains(req.URL.Path, "/images/") {
			wire = `{"code":0,"data":{"data":[],"usage":{"credit":5.71}}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
	})
	c.AccountCreditObserver = func(_ string, credit float64, has bool) {
		if !has {
			t.Fatal("未读到扣费回执")
		}
		credits = append(credits, credit)
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	_, rid, err := c.DesktopChatWithExpert(a, "expert")
	if err != nil || rid != id {
		t.Fatalf("expert id=%q err=%v", rid, err)
	}
	if _, err = c.ImageGenerations(a, map[string]any{"model": "image"}); err != nil {
		t.Fatal(err)
	}
	if len(credits) != 2 || credits[0] != 0.25 || credits[1] != 5.71 {
		t.Fatalf("expert/image credits=%v", credits)
	}
}
