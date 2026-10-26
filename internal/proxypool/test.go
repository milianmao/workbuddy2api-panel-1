// test.go 节点延迟测试：mihomo URLTest 打 generate_204，8s 超时。
// 成功回写 LatencyMS/CheckedAt 并清零 Fails；失败 Fails++ 并清空 LatencyMS
// （避免面板把旧延迟当成「当前可用」）。
package proxypool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
)

// Test 测单个节点延迟（毫秒）。节点不存在返回错误；拨号/握手失败返回错误并由
// 调用方决定展示（错误信息里带节点远端原因）。
func (p *Pool) Test(ctx context.Context, id string) (int, error) {
	p.mu.Lock()
	n, ok := p.nodes[id]
	if !ok {
		p.mu.Unlock()
		return 0, fmt.Errorf("proxy %s not found", id)
	}
	raw := n.Raw
	tag := n.Tag
	p.mu.Unlock()

	proxy, err := adapter.ParseProxy(raw)
	if err != nil {
		p.noteTestFail(id, 0)
		return 0, fmt.Errorf("parse node %s: %w", tag, err)
	}
	tctx, cancel := context.WithTimeout(ctx, latencyTimeout)
	defer cancel()
	start := time.Now()
	delay, err := proxy.URLTest(tctx, latencyTestURL, nil)
	if err != nil {
		p.noteTestFail(id, 0)
		return 0, err
	}
	ms := int(delay)
	if ms <= 0 {
		// URLTest 偶发 0（时钟精度）：用本地计时兜底，保证面板有值可显。
		ms = int(time.Since(start).Milliseconds())
	}
	p.noteTestOK(id, ms)
	return ms, nil
}

// TestBatch 并发批量测试（testConcurrency 上限），结果顺序与入参一致。
// ctx 取消时未跑完的项直接落 not-tested 错误。
func (p *Pool) TestBatch(ctx context.Context, ids []string) []TestResult {
	results := make([]TestResult, len(ids))
	sem := make(chan struct{}, testConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		results[i].ID = id
		// 取消后不再起新的测试：剩余项统一落错误。
		if ctx.Err() != nil {
			results[i].Error = "not tested: " + ctx.Err().Error()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			ms, err := p.Test(ctx, id)
			if err != nil {
				results[i].Error = err.Error()
				return
			}
			results[i].OK = true
			results[i].LatencyMS = ms
		}(i, id)
	}
	wg.Wait()
	return results
}

// noteTestOK / noteTestFail 回写测试观测（节点可能在测试期间被删：不存在则丢弃）。
func (p *Pool) noteTestOK(id string, ms int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return
	}
	n.LatencyMS = ms
	n.CheckedAt = time.Now().Unix()
	n.Fails = 0
	p.dirty.Store(true)
}

func (p *Pool) noteTestFail(id string, _ int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return
	}
	n.Fails++
	n.CheckedAt = time.Now().Unix()
	n.LatencyMS = 0
	p.dirty.Store(true)
}
