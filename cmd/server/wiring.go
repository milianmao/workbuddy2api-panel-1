package main

import (
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
	"github.com/misakano7545/workbuddy2api-panel/internal/nodebridge"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

type alertSource struct {
	pool *pool.Pool
	h    *server.Handler
}

func (s alertSource) Health(realm string) alert.Health {
	h := s.pool.RealmHealth(realm)
	return alert.Health{
		Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled,
		Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.InFlight,
	}
}

func (s alertSource) WAFActive() bool { return s.h.WAFActive() }

// proxyExitKeyOf 构造「账号 → 实际出口标识」闭包（WAF IP 级拦截的分组键）。
//
// 口径：#122 池里 fixed 绑定 = 该节点即出口（同节点 = 同 IP，互相连坐正确）；
// random / direct = 每次请求可能换出口，无法归组，统一返回 ""（各账号独立判定，
// 不做跨号连坐——随机出口下"同 IP"本就不成立）。
func proxyExitKeyOf(pp *proxypool.Manager) func(uid string) string {
	return func(uid string) string {
		if pp == nil {
			return ""
		}
		return pp.ExitKey(uid)
	}
}

// upstreamDialTimeout 暴露 upstream 的建连上限常量供装配期等值断言（口径单一
// 事实来源：proxypool.DefaultNodeDialTimeout 必须与它一致）。
func upstreamDialTimeout() time.Duration { return upstream.DialTimeout }

// migrateLegacyBindings 把 state.json 里的老 proxy_id（旧池的节点 ID）翻译成
// #122 池的 fixed 绑定：旧 ID → 桥上的 socks5 URL → Manager 节点 ID。
//
// 幂等：已迁移（ExitKey 已指向同一节点）的号直接跳过；找不到对应节点的旧绑定
// 原样留在 state.json 里（不删，回滚时旧版本还能用），只是不再生效。
func migrateLegacyBindings(p *pool.Pool, m *proxypool.Manager, br *nodebridge.Bridge) int {
	if p == nil || m == nil || br == nil {
		return 0
	}
	byURL := map[string]string{}
	for _, v := range m.Snapshot().Proxies {
		byURL[v.URL] = v.ID
	}
	moved := 0
	for _, st := range p.List() {
		if st.ProxyID == "" {
			continue
		}
		u, ok := br.URLOf(st.ProxyID)
		if !ok {
			continue
		}
		id := byURL[u]
		if id == "" || m.ExitKey(st.UID) == id {
			continue
		}
		if err := m.Bind(st.UID, proxypool.Binding{Mode: "fixed", ProxyID: id}); err != nil {
			log.Printf("WARN: [proxypool] 迁移旧代理绑定失败 uid=%s: %v", st.UID, err)
			continue
		}
		moved++
	}
	return moved
}
