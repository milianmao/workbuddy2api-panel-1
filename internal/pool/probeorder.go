package pool

// ModelProbeLimit 目录探针最多换几个账号尝试。
//
// 取 5：正常池里直连号排在最前面，第一个就成；5 个足够跨过几台坏出口节点，又不会
// 在「上游整体不可用」时把一次刷新放大成几十次探测。
const ModelProbeLimit = 5

// ModelProbeUIDs 返回用于拉取模型目录（/v1/models、面板「模型与档位」）的候选账号，
// 至多 n 个（n<=0 不限），顺序为「未绑固定代理（直连）→ 其余」，每段内保持 uid 升序。
//
// 为什么不能只取 AvailableUIDsForRealm[0]：目录探针虽然是只读 GET，却跑在那个账号的
// 出口上。账号若钉了固定代理，节点一坏，整份 CN 目录就拉不到——面板模型页只剩出图/出
// 视频，/v1/models 也跟着缩水；而 AvailableUIDsForRealm 是 uid 字典序，排第一的那个号
// 固定不变，于是同一个坏出口会把每一次刷新都打掉（2026-11-12 实案：5 个号钉在 4 台死
// 节点上，/v1/models 从 22 条掉到 4 条，并在 5 分钟负缓存里反复命中）。
//
// 未绑固定代理的账号走 realm 默认出口（本部署两域都是 direct），是最稳的探针；把它们
// 排到前面，绝大多数情况下第一次就成，坏出口只会在兜底位置被碰到。
//
// 注意：这里只做排序与截断，不看节点健康——健康表在 proxypool 进程内存里，且每个绑定
// 号都可能换节点。真正的兜底是调用方按返回顺序换下一个账号重试。
func (p *Pool) ModelProbeUIDs(realm string, n int) []string {
	uids := p.AvailableUIDsForRealm(realm)
	if len(uids) == 0 {
		return nil
	}
	p.mu.RLock()
	direct := make([]string, 0, len(uids))
	bound := make([]string, 0, len(uids))
	for _, uid := range uids {
		if e, ok := p.byUID[uid]; ok && e.proxyID == "" {
			direct = append(direct, uid)
			continue
		}
		bound = append(bound, uid)
	}
	p.mu.RUnlock()

	out := make([]string, 0, len(uids))
	out = append(out, direct...)
	out = append(out, bound...)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
