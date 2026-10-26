package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sync"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// checkAccountQuota 在各类真实模型调用出站前执行同一闸门；维护和领奖 RPC 不经过这里。
func (c *Client) checkAccountQuota(a *auth.Auth) error {
	if c.AccountQuotaCheck != nil {
		return c.AccountQuotaCheck(a.UID)
	}
	return nil
}

// modelCredit 只解析上游显式 credit，零值合法；图片回执无 token 时仍可结算。
func modelCredit(raw []byte) (float64, bool) {
	var env struct {
		Usage struct {
			Credit *float64 `json:"credit"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Usage.Credit == nil {
		return 0, false
	}
	v := *env.Usage.Credit
	return v, v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// modelUsageBody 旁观原始 SSE，保持字节不变；每条上游响应仅取最后有效用量快照结算一次。
// 结算发生于 DONE、EOF 或 Close，因此下一段续写准入前已能看到上一段扣费。
type modelUsageBody struct {
	io.ReadCloser
	mu       sync.Mutex
	pending  []byte
	credit   float64
	observed bool
	settled  bool
	observe  func(float64, bool)
}

// observeModelStream 未装配观察者时保持原始 Reader，不为独立命令行增加账本行为。
func (c *Client) observeModelStream(uid string, body io.ReadCloser) io.ReadCloser {
	if c.AccountCreditObserver == nil {
		return body
	}
	return &modelUsageBody{ReadCloser: body, observe: func(credit float64, observed bool) {
		c.AccountCreditObserver(uid, credit, observed)
	}}
}

// Read 只缓存当前未完成的 SSE 行，支持 JSON/换行跨任意读取边界，不吞掉读取错误。
func (b *modelUsageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.settled {
		data := p[:n]
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				b.pending = append(b.pending, data...)
				break
			}
			b.pending = append(b.pending, data[:i]...)
			b.line(b.pending)
			b.pending = b.pending[:0]
			data = data[i+1:]
		}
		if err != nil {
			b.finish()
		}
	}
	return n, err
}

// line 在持锁时观察用量；同一响应重复携带累计 usage 不会被当成多笔扣费。
func (b *modelUsageBody) line(line []byte) {
	if b.settled {
		return
	}
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(data, []byte("[DONE]")) {
		b.settle()
	} else if bytes.Contains(data, []byte(`"usage"`)) {
		if v, ok := modelCredit(data); ok {
			b.credit, b.observed = v, true
		}
	}
}

// finish 在持锁时消费没有换行的尾帧；Close 与 EOF 走同一幂等结算路径。
func (b *modelUsageBody) finish() {
	if !b.settled {
		b.line(b.pending)
		b.pending = nil
		b.settle()
	}
}

// settle 只回调一次，避免 Read 完成后 defer Close 重复入账；调用方须持锁。
func (b *modelUsageBody) settle() {
	if !b.settled {
		b.settled = true
		b.observe(b.credit, b.observed)
	}
}

// Close 保留底流关闭语义；即使提前取消没有尾帧，也要明确留下用量不完整标记。
func (b *modelUsageBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finish()
	return err
}
