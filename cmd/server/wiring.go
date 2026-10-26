package main

import (
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
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
// 为什么不能直接用 pool.AccountProxy（绑定意图）：TransportFor 在「节点不存在 /
// 已停用 / 解析失败」三种情况下都回落直连，这些账号的实际出口就是网关本机 IP。
// 若按绑定意图分组，它们会各自独立成组，而直连组只剩未绑定账号——WAF 的「短窗
// 多号同 IP」判定被稀释，真正的 IP 级拦截迟迟不触发。proxypoolSvc.ExitKey 与
// TransportFor 同一口径（空 = 直连），故这里只需一次查询。
func proxyExitKeyOf(p *pool.Pool, pp *proxypool.Pool) func(uid string) string {
	return func(uid string) string {
		if p == nil || pp == nil {
			return ""
		}
		return pp.ExitKey(p.AccountProxy(uid))
	}
}

// upstreamDialTimeout 暴露 upstream 的建连上限常量供装配期等值断言（口径单一
// 事实来源：proxypool.DefaultNodeDialTimeout 必须与它一致）。
func upstreamDialTimeout() time.Duration { return upstream.DialTimeout }
