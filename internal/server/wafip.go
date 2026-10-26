// wafip.go WAF IP 级拦截 fail-fast 状态机（任务书 waf-ip-failfast）。
//
// 背景（fork-scan-absorb T-1 / BulidH 实测）：WAF 403 拦的是网关出口 IP 而非
// 账号——3 个账号 1 秒内全 403。既有 ErrWafBlock 账号级软冷却（wafCooldownBase
// 起的有界退避）在 IP 级拦截时不够：轮转会把一次客户端请求放大 MaxRotate 倍，
// 同一出口 IP 继续打上游只会加重风控。
//
// 判定：ErrWafBlock 基础上的短窗多号计数——wafIPWindow（60s 滑动窗）内
// ≥ wafIPThreshold 个**不同** UID 接连命中 WAF 403 → 判定 IP 级拦截，激活至
// now+wafIPWindow。单号反复 403（账号级偶发）永不触发：只数不同号。
// 激活期内新命中不续期（保守：不做主动探测，窗口自然解除）。
//
// 归属层评估：放 server（Handler 局部）而非 pool——IP 级状态唯一消费者是
// chatCompletions 轮转循环（是否继续轮转），pool 是账号级记账层，跨 UID 语义
// 不属于任何账号；server 已有进程级状态先例 degradeGate（mu+until 同风格）。
// 账号级软冷却照常记账（applyErrorPolicy 不变），IP 级状态只改变「是否继续
// 轮转」——协同不叠加。进程内状态、重启清零（窗口 60s，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"
)

// wafIPWindow IP 级判定滑动窗 + 激活时长：窗内不同账号命中 WAF 403 达阈值即
// 判 IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s，
// 任务书「如 60s 滑动窗」口径）。
var wafIPWindow = 60 * time.Second

// wafIPThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复 403 永不触发（账号级偶发归软冷却管），两个不同号在 60s 内接连
// 被拦（同一出口 IP）已是 IP 级证据（BulidH 实测 3 号 1s 全拦，阈值 2 更早止损，
// 少放大一次轮转）。
const wafIPThreshold = 2

// wafIPGate WAF IP 级拦截状态机（Handler 内嵌，零值可用）。
type wafIPGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次 WAF 403 时刻（判定窗内，惰性剪枝）
	until time.Time            // IP 级拦截激活截止；零值 = 未激活
}

// noteWaf 记一次某账号的 WAF 403，返回记账后 IP 级拦截是否激活（调用方据此
// fail-fast 终止轮转，优先于 rotateBackoff 退避）。
//   - 已激活（now < until）：不续期、不记账（窗口期内不重置——保守自然解除）→ true；
//   - 未激活：记 hits[uid]=now（同号重复命中覆盖不累计，判定口径是「不同号数」），
//     剪掉窗外的过期命中；不同 UID 数达 wafIPThreshold → 激活到 now+wafIPWindow
//     （打一条 WARN 供观测），清空判定窗（解除后需全新命中重新判定，不叠旧账）。
func (g *wafIPGate) noteWaf(uid string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true // 激活期内新命中：不续期（自然解除语义，任务书第 3 条）
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > wafIPWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= wafIPThreshold {
		g.until = now.Add(wafIPWindow)
		log.Printf("WARN: [server] waf ip-level block: %d accounts hit waf 403 within %s, rotate fail-fast until %s", len(g.hits), wafIPWindow, g.until.Format(time.RFC3339))
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// active 报告 IP 级拦截是否激活（末端错误文案区分 IP 级/账号级措辞用）。
func (g *wafIPGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// ---------------------------------------------------------------------------
// 按出口分组
// ---------------------------------------------------------------------------

// 多出口后的分组语义（架构定案）：账号可绑定不同代理节点（= 不同出口 IP），
// IP 级拦截判定必须按出口分组——直连组（exitKey ""）与每个代理节点各自独立，
// 一个代理被拦**不能**误停其他出口的轮转；共用同一代理的账号仍在同一组内互相
// 连坐（这正是「同 IP」语义）。exitKey = 账号绑定的 proxy_id；server 不感知节点
// 本体，由 main 注入 ExitTagOf(uid) 闭包（nil = 全部归直连组，即旧行为）。

// exitTagOfUID 账号出口标识（ExitTagOf 未注入时恒 "" = 直连组）。
func (h *Handler) exitTagOfUID(uid string) string {
	if h.cfg.ExitTagOf == nil {
		return ""
	}
	return h.cfg.ExitTagOf(uid)
}

// wafGateForExit 返回某出口的 gate 实例（按需创建；实例数 ≤ 1+代理节点数，有界）。
func (h *Handler) wafGateForExit(exitKey string) *wafIPGate {
	h.wafMu.Lock()
	defer h.wafMu.Unlock()
	if h.wafIPGates == nil {
		h.wafIPGates = map[string]*wafIPGate{}
	}
	g, ok := h.wafIPGates[exitKey]
	if !ok {
		g = &wafIPGate{}
		h.wafIPGates[exitKey] = g
	}
	return g
}

// wafGateForUID 按账号解析出口并返回其 gate（未注入 ExitTagOf 时归直连组）。
func (h *Handler) wafGateForUID(uid string) *wafIPGate {
	_, g := h.wafGateOfUID(uid)
	return g
}

// wafGateOfUID 一次解析账号出口，返回 (exitKey, 该出口的 gate)。
// 轮转循环同时需要 exitKey（末端文案）与 gate（喂状态机），分开调用等于把
// ExitTagOf 闭包解析两遍（它可能要读节点表/持锁），故合并为单次解析。
func (h *Handler) wafGateOfUID(uid string) (string, *wafIPGate) {
	exit := h.exitTagOfUID(uid)
	return exit, h.wafGateForExit(exit)
}

// wafGateActive 报告某出口是否处于激活态。纯查询语义：不为未出现过的出口
// 新建 gate（末端文案判定不该扩张分组表）。
func (h *Handler) wafGateActive(exitKey string) bool {
	h.wafMu.Lock()
	g, ok := h.wafIPGates[exitKey]
	h.wafMu.Unlock()
	return ok && g.active()
}

// wafAnyActive 报告任一出口组处于 IP 级拦截激活态（/metrics 的 waf_ip_blocked
// 观测口径：全局视角）。
func (h *Handler) wafAnyActive() bool {
	h.wafMu.Lock()
	defer h.wafMu.Unlock()
	for _, g := range h.wafIPGates {
		if g.active() {
			return true
		}
	}
	return false
}
