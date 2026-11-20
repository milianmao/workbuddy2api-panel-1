package panel

import (
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/nodebridge"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// importAutoBindDefaultMax 每个节点允许绑定的账号数上限的缺省值。
//
// 5 不是随手定的：43 个机场节点只对应约 30 个 distinct 出口 IP，一个出口 IP 上堆太多
// 号会一起进上游风控（11140「账号存在异常行为」按出口/设备判定），所以节点级先留余量，
// 选点时再按出口 IP 二次摊开——多个节点会共用同一个出口 IP，按 IP 计数比按节点计数安全。
const importAutoBindDefaultMax = 5

// importAutoBindResult 导入后自动绑定的结果，直接进导入响应体。
//
// 单独记 bound/skipped/reason 是刻意的：导入之后最常见的追问是「为什么这个号没绑上」，
// 「没有空位 / 代理池没接 / 开关关着」必须能从响应里看出来，而不是去翻日志。
type importAutoBindResult struct {
	Enabled bool           `json:"enabled"`
	Cap     int            `json:"cap,omitempty"`
	Bound   int            `json:"bound"`
	Skipped int            `json:"skipped,omitempty"`
	DryRun  bool           `json:"dry_run,omitempty"`
	Nodes   map[string]int `json:"nodes,omitempty"`  // 节点展示名 → 本轮绑了几个
	Reason  string         `json:"reason,omitempty"` // 一个都没绑成时的原因
}

// importAutoBindSettings 读当前生效的自动绑定设置。
//
// 走 livecfg 快照（与 features.* 同一套热生效通路）：面板保存后下一次导入立刻按新值走。
// Live 为 nil（单元测试 / 未接线路径）时按缺省「开启 + 每节点 5」处理。
func (p *Panel) importAutoBindSettings() (enabled bool, cap int) {
	enabled, cap = true, importAutoBindDefaultMax
	if p.cfg.Live != nil {
		s := p.cfg.Live.Load()
		enabled = !s.ImportAutoBindOff
		if s.ImportAutoBindMaxPerProxy > 0 {
			cap = s.ImportAutoBindMaxPerProxy
		}
	}
	return enabled, cap
}

// importBindCandidate 一个可绑定节点的选点视图。
type importBindCandidate struct {
	id      string
	label   string
	ip      string // 出口 IP；空 = 还不知道（出口探测没跑过）
	count   int    // 节点上已绑定的账号数
	ipCount int    // 同出口 IP 上已绑定的账号数（跨节点累加）
	healthy bool
	latency int64
}

// autoBindImported 给刚导入的新号各挑一个代理节点固定绑上。
//
// 选点顺序（与线上运维口径一致）：
//  1. 只考虑 enabled 且当前 fixed 绑定数 < cap 的节点——满额的直接排除，绝不超发；
//  2. 优先「同出口 IP 上账号最少」的节点（多个节点可能共用同一出口 IP）；
//  3. 再看节点自身绑定数、健康状态、延迟（不知道延迟的排在有延迟的后面）。
//
// 已经有绑定的号（覆盖导入的旧号）一个都不动；没有空位就留着不绑（走 realm 默认 /
// 随机池 / 直连），并在结果里说明——不为了「每个号都有代理」突破上限。
func (p *Panel) autoBindImported(list []importAcct) importAutoBindResult {
	enabled, cap := p.importAutoBindSettings()
	res := importAutoBindResult{Enabled: enabled, Cap: cap}
	if !enabled {
		res.Skipped = len(list)
		res.Reason = "panel.import_auto_bind_proxy = false（已关闭）"
		return res
	}
	if p.cfg.ProxyPool == nil {
		res.Skipped = len(list)
		res.Reason = "代理池未启用（proxypool 为空）"
		return res
	}
	snap := p.cfg.ProxyPool.Snapshot()

	// 出口 IP 上的账号总数：同一 IP 可能被多个节点共用（同机房多入口），
	// 只有把这些节点加起来才是「上游看到的同一个出口」。
	ipOf := make(map[string]string, len(snap.Proxies))
	ipBound := map[string]int{}
	for _, v := range snap.Proxies {
		ip := strings.TrimSpace(v.Exit.IP)
		if ip == "" {
			continue
		}
		ipOf[v.ID] = ip
		ipBound[ip] += v.BoundCount
	}

	cands := make([]*importBindCandidate, 0, len(snap.Proxies))
	for _, v := range snap.Proxies {
		if !v.Enabled || v.BoundCount >= cap {
			continue
		}
		label := p.proxyLabel(v)
		// 机场会把「距离下次重置剩余：27 天」这类套餐文案行一起塞进节点列表：它带的是
		// 同一台机器的真配置、也拨得通，但对号来说是个假名字（线上真绑上过一次），
		// 这种条目不能当选点。
		if nodebridge.IsInfoOnlyTag(label) {
			continue
		}
		ip := ipOf[v.ID]
		cands = append(cands, &importBindCandidate{
			id: v.ID, label: label, ip: ip, count: v.BoundCount,
			ipCount: ipBound[ip], healthy: v.Status == "healthy", latency: v.LatencyMS,
		})
	}
	if len(cands) == 0 {
		res.Skipped = len(list)
		res.Reason = "没有空位：所有节点都已停用或绑定满额（每节点上限 " + strconv.Itoa(cap) + "）"
		return res
	}

	less := func(a, b *importBindCandidate) bool {
		if a.healthy != b.healthy {
			return a.healthy
		}
		if a.ipCount != b.ipCount {
			return a.ipCount < b.ipCount
		}
		if a.count != b.count {
			return a.count < b.count
		}
		la, lb := a.latency, b.latency
		if la <= 0 {
			la = 1 << 30
		}
		if lb <= 0 {
			lb = 1 << 30
		}
		if la != lb {
			return la < lb
		}
		return a.id < b.id
	}

	for _, ia := range list {
		// 覆盖导入（号原本就在池里）继承原有绑定：只在完全没有绑定时才补。
		if _, bound := snap.Bindings[ia.uid]; bound {
			continue
		}
		sort.SliceStable(cands, func(i, j int) bool { return less(cands[i], cands[j]) })
		var pick *importBindCandidate
		for _, c := range cands {
			if c.count < cap {
				pick = c
				break
			}
		}
		if pick == nil {
			res.Skipped++
			continue
		}
		if err := p.cfg.ProxyPool.Bind(ia.uid, proxypool.Binding{Mode: "fixed", ProxyID: pick.id}); err != nil {
			res.Skipped++
			log.Printf("WARN: panel: import auto-bind uid=%s proxy=%s 绑定失败: %v", ia.uid, pick.id, err)
			continue
		}
		// 本批内自己维护计数：后面的号必须看到前面已经占掉的位子，
		// 否则同一批 100 个号会全压在前几个节点上。
		pick.count++
		if pick.ip != "" {
			for _, c := range cands {
				if c.ip == pick.ip {
					c.ipCount++
				}
			}
		}
		res.Bound++
		if res.Nodes == nil {
			res.Nodes = map[string]int{}
		}
		res.Nodes[pick.label]++
		log.Printf("panel: import auto-bind uid=%s proxy=%s tag=%s (%d/%d)", ia.uid, pick.id, pick.label, pick.count, cap)
	}
	if res.Reason == "" && res.Skipped > 0 {
		res.Reason = "本批把可用节点占满后仍有 " + strconv.Itoa(res.Skipped) + " 个号没绑（每节点上限 " +
			strconv.Itoa(cap) + "）"
	}
	return res
}

// proxyLabel 节点的展示名：机场订阅里的 tag 优先，其次节点自带 name，最后退 id。
func (p *Panel) proxyLabel(v proxypool.View) string {
	if p.cfg.NodeBridge != nil {
		if m, ok := p.cfg.NodeBridge.Meta(v.URL); ok && strings.TrimSpace(m.Tag) != "" {
			return m.Tag
		}
	}
	if s := strings.TrimSpace(v.Name); s != "" {
		return s
	}
	return v.ID
}
