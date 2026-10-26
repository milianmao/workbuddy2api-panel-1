// proxy.go 面板代理池 API：节点列表/导入/删除/启停/测延迟、订阅增删同步、
// 账号绑定代理。本层只做装配与视图合并（pool ↔ proxypool），业务规则在
// internal/proxypool；账号绑定关系落 pool（stateAccount.ProxyID 持久化）。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// proxyImportMaxBytes 导入请求体上限。该接口与网关同进程（共用内存），一次粘贴
// 大文件足以打爆进程；4 MiB 对「几千行 URI / 一份完整 Clash YAML」绰绰有余。
const proxyImportMaxBytes = 4 << 20

// proxyItem 列表行：裁剪后的节点视图 + accounts（绑定该节点的账号数 = 同一出口 IP
// 的账号数）。
//
// 为什么不用 proxypool.Node 内嵌：Node.Raw 是完整出站配置（password/uuid/obfs/…），
// 列表每 5s 轮询一次，凭据不该进这个响应。前端只用下列字段。
type proxyItem struct {
	ID        string `json:"id"`
	Tag       string `json:"tag"`
	Type      string `json:"type"`
	Server    string `json:"server"`
	Port      int    `json:"port"`
	SubID     string `json:"sub_id"`
	Enabled   bool   `json:"enabled"`
	LatencyMS int    `json:"latency_ms"`
	CheckedAt int64  `json:"checked_at"`
	Fails     int    `json:"fails"`
	Accounts  int    `json:"accounts"`
}

// proxiesList GET /panel/api/proxies → {ok, proxies:[...], subs:[...]}。
func (p *Panel) proxiesList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	nodes := p.cfg.Proxypool.List()
	subs := p.cfg.Proxypool.Subs()
	counts := p.proxyAccountCounts()
	items := make([]proxyItem, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, proxyItem{
			ID:        n.ID,
			Tag:       n.Tag,
			Type:      n.Type,
			Server:    n.Server,
			Port:      n.Port,
			SubID:     n.SubID,
			Enabled:   n.Enabled,
			LatencyMS: n.LatencyMS,
			CheckedAt: n.CheckedAt,
			Fails:     n.Fails,
			Accounts:  counts[n.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxies": items, "subs": subs})
}

// proxyAccountCounts 统计各节点被多少账号绑定（口径：池内 ProxyID 直方图）。
func (p *Panel) proxyAccountCounts() map[string]int {
	counts := map[string]int{}
	if p.cfg.Pool == nil {
		return counts
	}
	for _, st := range p.cfg.Pool.List() {
		if st.ProxyID != "" {
			counts[st.ProxyID]++
		}
	}
	return counts
}

// decodeLimitedJSON 带上限的 JSON 解码：超限返回 errBodyTooLarge（调用方 413）。
func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return err
	}
	return nil
}

// errBodyTooLarge 请求体超过上限（413）。
var errBodyTooLarge = errors.New("request body too large")

// writeDecodeErr 统一解码错误出口：超限 413，其余 400。
func writeDecodeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (limit %d bytes)", proxyImportMaxBytes))
		return
	}
	writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
}

// proxiesImport POST /panel/api/proxies {"text":"<多行：URI 或 clash yaml>"}
// → {ok,total,imported,skipped,errors}。请求体与单次节点数均有上限（超限的节点数
// 计入 skipped，errors 里给出原因）。
func (p *Panel) proxiesImport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeLimitedJSON(w, r, proxyImportMaxBytes, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	res, err := p.cfg.Proxypool.AddRaw(req.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: proxies import total=%d imported=%d skipped=%d errors=%d", res.Total, res.Imported, res.Skipped, len(res.Errors))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": res.Total, "imported": res.Imported, "skipped": res.Skipped, "errors": emptyIfNil(res.Errors)})
}

// proxyDelete DELETE /panel/api/proxies/{id} → {ok:true}（绑定账号自动解绑）。
func (p *Panel) proxyDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	id := r.PathValue("id")
	unbound, err := p.cfg.Proxypool.Delete(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("panel: proxy %s deleted, unbound %d account(s)", id, len(unbound))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxyToggle POST /panel/api/proxies/{id}/toggle → {ok:true,enabled}。
// 停用后该节点绑定的账号回落直连（TransportFor → nil）。
func (p *Panel) proxyToggle(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	enabled, err := p.cfg.Proxypool.Toggle(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

// proxyTest POST /panel/api/proxies/{id}/test → {ok:true,latency_ms}；
// 测试失败 502 {ok:false,error}；节点不存在 404。
func (p *Panel) proxyTest(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	id := r.PathValue("id")
	if _, ok := p.cfg.Proxypool.NodeView(id); !ok {
		writeErr(w, http.StatusNotFound, "proxy not found")
		return
	}
	ms, err := p.cfg.Proxypool.Test(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": ms})
}

// proxiesTestSel POST /panel/api/proxies/test_sel {"ids":[...]}
// → {ok:true,results:[{id,ok,latency_ms,error}]}（并发批量，顺序与入参一致）。
func (p *Panel) proxiesTestSel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": p.cfg.Proxypool.TestBatch(r.Context(), req.IDs)})
}

// subscriptionCreate POST /panel/api/subscriptions {"name":"可选","url":"https://..."}
// → 创建 + 立即同步，返回 {ok,sub,total,imported,skipped,errors}。
func (p *Panel) subscriptionCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	sub, err := p.cfg.Proxypool.AddSubscription(req.Name, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p.syncSubscription(w, r, sub.ID)
}

// subscriptionSync POST /panel/api/subscriptions/{id}/sync → 同 create 形状。
func (p *Panel) subscriptionSync(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	p.syncSubscription(w, r, r.PathValue("id"))
}

// syncSubscription 同步并写公共响应（sub 视图 + 导入台账）。
// 订阅不存在 → 404（与 DELETE 一致）；拉取/解析失败 → 502（上游订阅端点的问题）。
func (p *Panel) syncSubscription(w http.ResponseWriter, r *http.Request, subID string) {
	res, err := p.cfg.Proxypool.Sync(r.Context(), subID)
	if err != nil {
		if errors.Is(err, proxypool.ErrSubNotFound) {
			writeErr(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	sub, ok := p.subView(subID)
	if !ok {
		writeErr(w, http.StatusNotFound, "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sub": sub,
		"total": res.Total, "imported": res.Imported, "skipped": res.Skipped, "errors": emptyIfNil(res.Errors),
	})
}

// emptyIfNil 契约口径：errors 恒为数组（null 会破坏前端 .map 消费）。
func emptyIfNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// subView 订阅视图（同步后取最新 count）。
func (p *Panel) subView(id string) (proxypool.SubView, bool) {
	for _, s := range p.cfg.Proxypool.Subs() {
		if s.ID == id {
			return s, true
		}
	}
	return proxypool.SubView{}, false
}

// subscriptionDelete DELETE /panel/api/subscriptions/{id}
// → {ok:true,removed_nodes,unbound_accounts}。
func (p *Panel) subscriptionDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	removed, unbound, err := p.cfg.Proxypool.DeleteSubscription(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("panel: subscription %s deleted, removed %d node(s), unbound %d account(s)",
		r.PathValue("id"), len(removed), len(unbound))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "removed_nodes": len(removed), "unbound_accounts": len(unbound),
	})
}

// accountBindProxy POST /panel/api/accounts/{uid}/bind_proxy
// body {"proxy_id":"abc"} 或 {"proxy_id":""}（解绑）→ {ok:true,proxy_tag}。
//
// 停用节点：**允许预绑定**（绑定意图持久化，节点重新启用即生效；与「停用节点 =
// 绑定它的账号回落直连」的既有语义一致，且不会因一次误停用丢掉运维意图）。
// 前端据此把停用节点显示为「已失效」而非禁止操作。
func (p *Panel) accountBindProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		ProxyID string `json:"proxy_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	proxyTag := ""
	if req.ProxyID != "" {
		if p.cfg.Proxypool == nil {
			writeErr(w, http.StatusNotImplemented, "proxypool not available")
			return
		}
		nv, ok := p.cfg.Proxypool.NodeView(req.ProxyID)
		if !ok {
			writeErr(w, http.StatusNotFound, "proxy not found")
			return
		}
		proxyTag = nv.Tag
	}
	if !p.cfg.Pool.SetAccountProxy(uid, req.ProxyID) {
		// 幂等重绑或账号不存在都返回 false；上面已排除不存在 → 视为无变更，照常成功。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_tag": proxyTag})
		return
	}
	log.Printf("panel: bind_proxy uid=%s proxy_id=%q", uid, req.ProxyID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_tag": proxyTag})
}

// accountProxyViews 批量解出账号的 proxy_tag/proxy_ok（overview 注入用）：
// proxy_tag = 绑定节点名（未绑/节点已删为 ""）；proxy_ok = 绑定节点仍存在且启用。
// list 由调用方传入已拉到的 pool.Status 切片：Pool.List() 是 O(n log n) 的排序
// 快照，overview 本来就要一份，这里再拉一次纯属白做。
func (p *Panel) accountProxyViews(list []pool.Status) map[string]proxyView {
	views := map[string]proxyView{}
	if p.cfg.Proxypool == nil || p.cfg.Pool == nil {
		return views
	}
	for _, st := range list {
		if st.ProxyID == "" {
			continue
		}
		v := proxyView{}
		if nv, ok := p.cfg.Proxypool.NodeView(st.ProxyID); ok {
			v.ProxyTag = nv.Tag
			v.ProxyOK = nv.Enabled
		}
		views[st.ProxyID] = v
	}
	return views
}

// proxyView overview 里每个账号的代理展示字段。
// 不带 ProxyID：那是 pool.Status 内联出来的原值，这里再存一份没人读（只增重复）。
type proxyView struct {
	ProxyTag string
	ProxyOK  bool
}

// overviewAccounts 账号池视图 + 代理注入（proxy_id/proxy_tag/proxy_ok）+ 今日
// 实测扣费积分注入。
func (p *Panel) overviewAccounts() []accountOverview {
	list := p.cfg.Pool.List()
	var views map[string]proxyView
	if p.cfg.Proxypool != nil {
		views = p.accountProxyViews(list)
	}
	// 今日积分一次算完再按下标查表：overview 是 5 秒轮询接口，不能每号各扫一遍桶。
	// Usage 为 nil（未装配记录器）时 credits 保持 nil，四行契约退化为「字段全缺席」。
	var credits map[string]usage.DayCredits
	if p.cfg.Usage != nil {
		credits = p.cfg.Usage.TodayCreditsByUID(time.Now())
	}
	out := make([]accountOverview, len(list))
	for i, st := range list {
		item := accountOverview{Status: st}
		// 未绑定代理的账号直接跳过查表（accountProxyViews 也不收空 ProxyID，
		// 两层都挡着，overview 不会因为空键读到别人的节点）。
		if st.ProxyID != "" {
			if v, ok := views[st.ProxyID]; ok {
				item.ProxyTag = v.ProxyTag
				item.ProxyOK = v.ProxyOK
			}
		}
		// 映射见 CreditsUsedToday 的契约：没有记录器 → 缺席；今天没桶 → 0；
		// 有桶且观测到回执 → 合计；有桶但零回执 → 缺席（花费未知，不能写 0）。
		//
		// 分支判据是「记录器在不在」，不是「结果 map 空不空」：TodayCreditsByUID 在
		// 今天一条桶都没有时返回 nil，那和「压根没装配记录器」是两件事——混在一起就会把
		// 整池空闲（夜间 / 刚部署 / 单号池）的所有行都打成「—」，而契约要求它们显示 0。
		// 这里查的 credits 可能是 nil：nil map 的取值恒为 miss，正好走到 else → 0。
		if p.cfg.Usage != nil {
			if dc, ok := credits[st.UID]; ok {
				if dc.Observed {
					v := dc.Credits
					item.CreditsUsedToday = &v
				}
				// 有桶但零回执：花费未知，保持缺席（「—」），不许写 0。
			} else {
				zero := 0.0
				item.CreditsUsedToday = &zero
			}
		}
		out[i] = item
	}
	return out
}

// accountOverview overview 账号行：pool.Status 内联展开 + 代理注入字段 + 今日积分。
type accountOverview struct {
	pool.Status
	ProxyTag string `json:"proxy_tag,omitempty"`
	ProxyOK  bool   `json:"proxy_ok"`
	// CreditsUsedToday 本地自然日（宿主本地日，见 usage.TodayCreditsByUID）内该
	// 账号在本网关的实测扣费积分。pointer + omitempty 表达四种情形：
	//   - 未装配用量记录器（Config.Usage == nil）→ 缺席，前端「—」；
	//   - 记录器在、今天没有它的桶 → 0（本网关今天确实没为它花额度）；
	//   - 今天有桶且至少一次 credit 回执 → CR 合计（免费/限免模型合法为 0）；
	//   - 今天有桶但一次回执都没拿到 → 缺席，前端「—」。
	// 最后一条是本字段的要点：「—」= 无法统计，不是 0。没观测到 credit 就断言
	// 「没花钱」正是 usage 包注释禁掉的事（CRN 计数器就是为此存在）。
	CreditsUsedToday *float64 `json:"credits_used_today,omitempty"`
}
