package pool

// ModelProbeLimit 目录探针最多换几个账号尝试。
//
// 取 8：正常池里直连号排在最前面，第一个就成；但排在最前的若全是「钉在坏节点上的号」，
// 上限必须留足余量——否则候选被坏出口吃满，明明有健康直连号也会整份目录失败
// （2026-11-12 实案池里有 5 个号钉在 4 台死节点上）。8 也不会在「上游整体不可用」时
// 把一次刷新放大成几十次探测。
const ModelProbeLimit = 8

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
	return p.ModelProbeUIDsWith(realm, n, nil)
}

// ModelProbeUIDsWith 同 ModelProbeUIDs，但用外部谓词判断「这个号是不是直连」。
//
// 为什么需要：出口真相在 proxypool 的绑定表里，pool 条目上的 proxyID 只是镜像，可能
// 滞后或缺失（绑定在 data/proxy-pool.json 里、条目从 state.json 恢复时就不一致）。
// 2026-11-12 实案里正是这种情况：5 个号在 proxypool 里钉着死节点，镜像却是空，于是
// 它们被当成「直连」排到最前，每次刷新都先撞 3 次超时。调用方传 nil 则退回镜像口径。
func (p *Pool) ModelProbeUIDsWith(realm string, n int, isDirect func(uid string) bool) []string {
	uids := p.AvailableUIDsForRealm(realm)
	if len(uids) == 0 {
		return nil
	}
	if isDirect == nil {
		p.mu.RLock()
		mirror := make(map[string]string, len(uids))
		for _, uid := range uids {
			if e, ok := p.byUID[uid]; ok {
				mirror[uid] = e.proxyID
			}
		}
		p.mu.RUnlock()
		isDirect = func(uid string) bool { return mirror[uid] == "" }
	}
	direct := make([]string, 0, len(uids))
	bound := make([]string, 0, len(uids))
	for _, uid := range uids {
		if isDirect(uid) {
			direct = append(direct, uid)
			continue
		}
		bound = append(bound, uid)
	}

	out := make([]string, 0, len(uids))
	out = append(out, direct...)
	out = append(out, bound...)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
