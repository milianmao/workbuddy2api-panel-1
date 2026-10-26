// proxy.go 账号 ↔ 出站代理绑定（proxypool 节点）。
//
// 定位：池只存「账号 → 节点 ID」的绑定关系（运维意图，持久化），不感知节点本体——
// 节点解析/拨号在 proxypool，分层硬约束 pool 不 import proxypool/upstream。
// 绑定语义：
//   - 同一节点可被多个账号绑定 = 共用同一出口 IP；
//   - 节点被删除时由 main 注入的回调（proxypool.SetUnbindCallback → ClearProxyIfMatch）
//     自动解绑，避免悬空绑定；
//   - 绑定值置空 = 解绑（回落直连）。
package pool

import "sort"

// SetAccountProxy 绑定账号到出站代理节点；id 为空 = 解绑（回落直连）。
// 节点是否存在由 proxypool 侧判定（面板先校验再调）。返回是否确有变更。
func (p *Pool) SetAccountProxy(uid, id string) bool {
	if uid == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.proxyID == id {
		return false // 幂等
	}
	e.proxyID = id
	p.dirty.Store(true)
	return true
}

// AccountProxy 返回账号绑定的代理节点 ID（"" = 未绑定/直连；账号不存在也返回 ""）。
func (p *Pool) AccountProxy(uid string) string {
	if uid == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return ""
	}
	return e.proxyID
}

// ClearProxyIfMatch 批量解绑：解除所有绑定到 ids 中任一节点的账号（节点被删时
// 由 proxypool 回调调用）。返回被解绑的 uid 列表（升序，输出稳定）；无变更返回 nil。
func (p *Pool) ClearProxyIfMatch(ids map[string]bool) []string {
	if len(ids) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var unbound []string
	for uid, e := range p.byUID {
		if e.proxyID != "" && ids[e.proxyID] {
			e.proxyID = ""
			unbound = append(unbound, uid)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	sort.Strings(unbound)
	p.dirty.Store(true)
	return unbound
}
