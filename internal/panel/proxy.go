// proxy.go 面板代理池 API（#122 引擎版）。
//
// 分层：路由/健康/绑定全在上游 internal/proxypool.Manager（#122）；本层只做
// 「Manager 视图 + 机场订阅桥」的装配——订阅里的 vless/hysteria2 节点由
// internal/nodebridge 在本机开 socks5 监听，再以 socks5:// URL 形式喂给 Manager。
//
// UI 契约保持不变（前端仍按 proxy_id 绑定/解绑）：绑节点 = fixed，解绑 = random，
// 显式直连 = direct（面板暂未暴露，接口支持）。
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

// proxyImportMaxBytes 导入请求体上限（同进程，防一次粘贴打爆内存）。
const proxyImportMaxBytes = 4 << 20

// proxyItem 列表行（前端只用这些字段；不带凭据）。
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

// subItem 订阅行。
type subItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Count      int    `json:"count"`
	LastSyncAt int64  `json:"last_sync_at"`
}

// proxiesList GET /panel/api/proxies → {ok, proxies:[...], subs:[...]}。
func (p *Panel) proxiesList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	snap := p.cfg.Proxypool.Snapshot()
	items := make([]proxyItem, 0, len(snap.Proxies))
	for _, v := range snap.Proxies {
		it := proxyItem{
			ID:        v.ID,
			Tag:       v.Name,
			Type:      v.Protocol,
			Enabled:   v.Enabled,
			LatencyMS: int(v.LatencyMS),
			CheckedAt: v.CheckedAt.Unix(),
			Fails:     v.Failures,
			Accounts:  v.BoundCount,
		}
		if p.cfg.NodeBridge != nil {
			if m, ok := p.cfg.NodeBridge.Meta(v.URL); ok {
				it.Tag, it.Type, it.Server, it.Port, it.SubID = m.Tag, m.Type, m.Server, m.Port, m.SubID
			}
		}
		items = append(items, it)
	}
	subs := []subItem{}
	if p.cfg.NodeBridge != nil {
		for _, s := range p.cfg.NodeBridge.Subs() {
			n := 0
			for _, it := range items {
				if it.SubID == s.ID {
					n++
				}
			}
			subs = append(subs, subItem{ID: s.ID, Name: s.Name, URL: s.URL, Count: n, LastSyncAt: s.LastSync})
		}
	}
	realms := snap.Realms
	if realms == nil {
		realms = map[string]proxypool.Binding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxies": items, "subs": subs, "realms": realms})
}

// proxiesImport POST /panel/api/proxies {text} → {ok,total,imported,skipped,errors}。
// 只接受代理 URL（http/https/socks5/socks5h），一行一个——机场 Clash 订阅请走订阅接口。
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
	res, err := p.cfg.Proxypool.Import(req.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	total, imported, skipped := len(res), 0, 0
	errs := []string{}
	for _, one := range res {
		switch one.Status {
		case "added":
			imported++
		case "duplicate":
			skipped++
		default:
			skipped++
			if one.Error != "" {
				errs = append(errs, one.Error)
			}
		}
	}
	log.Printf("panel: proxies import total=%d imported=%d skipped=%d errors=%d", total, imported, skipped, len(errs))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": total, "imported": imported, "skipped": skipped, "errors": errs})
}

// proxyDelete DELETE /panel/api/proxies/{id}（绑定该节点的账号自动解绑）。
func (p *Panel) proxyDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	if err := p.cfg.Proxypool.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxyToggle POST /panel/api/proxies/{id}/toggle → {ok,enabled}。
func (p *Panel) proxyToggle(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	id := r.PathValue("id")
	var cur bool
	found := false
	for _, v := range p.cfg.Proxypool.Snapshot().Proxies {
		if v.ID == id {
			cur, found = v.Enabled, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "proxy not found")
		return
	}
	next := !cur
	if err := p.cfg.Proxypool.Update(id, proxypool.Input{Enabled: &next}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": next})
}

// proxyTest POST /panel/api/proxies/{id}/test —— 异步探测，结果由列表轮询回显。
func (p *Panel) proxyTest(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	if err := p.cfg.Proxypool.RequestCheck(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxiesTestSel POST /panel/api/proxies/test_sel {ids} —— 批量异步探测。
func (p *Panel) proxiesTestSel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeLimitedJSON(w, r, 64<<10, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	results := make([]map[string]any, 0, len(req.IDs))
	for _, id := range req.IDs {
		if err := p.cfg.Proxypool.RequestCheck(id); err != nil {
			results = append(results, map[string]any{"id": id, "ok": false, "error": err.Error()})
			continue
		}
		results = append(results, map[string]any{"id": id, "ok": true})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "async": true})
}

// subscriptionCreate POST /panel/api/subscriptions {name,url}。
func (p *Panel) subscriptionCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.NodeBridge == nil {
		writeErr(w, http.StatusNotImplemented, "node bridge not available")
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := decodeLimitedJSON(w, r, 8<<10, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if req.Name == "" {
		req.Name = "订阅"
	}
	sub, err := p.cfg.NodeBridge.AddSub(r.Context(), req.Name, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	added := p.importBridgeNodes()
	rec := p.reconcileBridgePool()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": sub.ID, "imported": added,
		"removed": rec.Removed, "remapped": len(rec.Remapped), "unbound": len(rec.Unbound)})
}

// subscriptionSync POST /panel/api/subscriptions/{id}/sync。
func (p *Panel) subscriptionSync(w http.ResponseWriter, r *http.Request) {
	if p.cfg.NodeBridge == nil {
		writeErr(w, http.StatusNotImplemented, "node bridge not available")
		return
	}
	id := r.PathValue("id")
	if _, err := p.cfg.NodeBridge.Sync(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	added := p.importBridgeNodes()
	rec := p.reconcileBridgePool()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": added,
		"removed": rec.Removed, "remapped": len(rec.Remapped), "unbound": len(rec.Unbound)})
}

// subscriptionDelete DELETE /panel/api/subscriptions/{id}。
func (p *Panel) subscriptionDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.NodeBridge == nil {
		writeErr(w, http.StatusNotImplemented, "node bridge not available")
		return
	}
	if err := p.cfg.NodeBridge.DelSub(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// importBridgeNodes 把桥当前的 socks5 出口推进上游池（幂等：同 URL 视为 duplicate）。
func (p *Panel) importBridgeNodes() int {
	if p.cfg.NodeBridge == nil || p.cfg.Proxypool == nil {
		return 0
	}
	text := p.cfg.NodeBridge.Lines()
	if text == "" {
		return 0
	}
	res, err := p.cfg.Proxypool.Import(text)
	if err != nil {
		log.Printf("panel: import bridge nodes failed: %v", err)
		return 0
	}
	added := 0
	for _, one := range res {
		if one.Status == "added" {
			added++
		}
	}
	if added > 0 {
		log.Printf("panel: bridge imported %d node(s) into proxy pool", added)
	}
	p.reconcileBridgePool()
	return added
}

// reconcileBridgePool 让池跟订阅当前的真实出口集对齐：删掉已消失的节点，并把
// 指向它们的 fixed 绑定按节点名迁到同名新节点（迁不了就解绑回随机池）。
// 触发点：订阅新建/同步之后 —— Import 只增不删，不清就会留下永远探不通的僵尸行。
func (p *Panel) reconcileBridgePool() proxypool.ReconcileResult {
	var res proxypool.ReconcileResult
	if p.cfg.NodeBridge == nil || p.cfg.Proxypool == nil {
		return res
	}
	res, err := p.cfg.Proxypool.Reconcile(p.cfg.NodeBridge.Keep(), p.cfg.NodeBridge.TagForURL)
	if err != nil {
		log.Printf("panel: proxy pool reconcile failed: %v", err)
		return proxypool.ReconcileResult{}
	}
	if res.Skipped != "" {
		log.Printf("panel: proxy pool reconcile skipped: %s", res.Skipped)
		return res
	}
	if res.Removed > 0 || len(res.Remapped) > 0 || len(res.Unbound) > 0 {
		log.Printf("panel: pool reconcile removed=%d remapped=%d unbound=%d",
			res.Removed, len(res.Remapped), len(res.Unbound))
	}
	return res
}

// realmProxy PUT /panel/api/proxies/realm/{realm}
// body {"mode":"direct"} | {"mode":"random"} | {"mode":"fixed","proxy_id":"..."}
// 分区默认出口（issue #39 P3）：账号没显式绑定时按 realm 取它。
func (p *Panel) realmProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Proxypool == nil {
		writeErr(w, http.StatusNotImplemented, "proxypool not available")
		return
	}
	realm := r.PathValue("realm")
	if realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm must be cn or global")
		return
	}
	var body struct {
		ProxyID string `json:"proxy_id"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	bind := proxypool.Binding{Mode: "random"}
	switch body.Mode {
	case "random":
	case "direct":
		bind = proxypool.Binding{Mode: "direct"}
	case "fixed":
		if body.ProxyID == "" {
			writeErr(w, http.StatusBadRequest, "proxy_id required")
			return
		}
		bind = proxypool.Binding{Mode: "fixed", ProxyID: body.ProxyID}
	default:
		writeErr(w, http.StatusBadRequest, "mode must be direct, random or fixed")
		return
	}
	if err := p.cfg.Proxypool.BindRealm(realm, bind); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountBindProxy POST /panel/api/accounts/{uid}/bind_proxy
// body {"proxy_id":"abc"}（绑定固定出口）/ {"proxy_id":""}（解绑=默认随机）
// / {"mode":"direct"}（强制直连）。
func (p *Panel) accountBindProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil || p.cfg.Proxypool == nil {
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
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	bind := proxypool.Binding{Mode: "random"}
	proxyTag := ""
	if req.Mode == "direct" {
		bind = proxypool.Binding{Mode: "direct"}
	} else if req.ProxyID != "" {
		if p.cfg.NodeBridge != nil {
			url := ""
			for _, v := range p.cfg.Proxypool.Snapshot().Proxies {
				if v.ID == req.ProxyID {
					if m, ok := p.cfg.NodeBridge.Meta(v.URL); ok {
						proxyTag = m.Tag
					}
					url = v.URL
					break
				}
			}
			if url == "" {
				writeErr(w, http.StatusNotFound, "proxy not found")
				return
			}
		}
		bind = proxypool.Binding{Mode: "fixed", ProxyID: req.ProxyID}
	}
	if err := p.cfg.Proxypool.Bind(uid, bind); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: bind_proxy uid=%s mode=%s proxy_id=%q", uid, bind.Mode, bind.ProxyID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_tag": proxyTag, "mode": bind.Mode})
}

// proxyView overview 里每个账号的代理展示字段。
type proxyView struct {
	ProxyTag string
	ProxyOK  bool
	// ProxyID 固定出口时是节点 id（前端绑定弹窗的预选值）；direct/random 为空。
	ProxyID string
}

// accountProxyViews 批量解出账号的 proxy_tag/proxy_ok（overview 注入用）。
// 绑定关系在上游池里（bindings[uid]），节点名经桥反查。
// proxyNodeTags 节点 id → 面板显示名 / 是否启用。桥能反查就用机场里的节点名，
// 反查不到（陈旧 URL）退回池里的原始名。
func (p *Panel) proxyNodeTags(snap proxypool.Snapshot) (map[string]string, map[string]bool) {
	tags := map[string]string{}
	enabled := map[string]bool{}
	for _, v := range snap.Proxies {
		name := v.Name
		if p.cfg.NodeBridge != nil {
			if m, ok := p.cfg.NodeBridge.Meta(v.URL); ok {
				name = m.Tag
			}
		}
		tags[v.ID], enabled[v.ID] = name, v.Enabled
	}
	return tags, enabled
}

func (p *Panel) accountProxyViews(snap proxypool.Snapshot) map[string]proxyView {
	views := map[string]proxyView{}
	tags, enabled := p.proxyNodeTags(snap)
	for uid, b := range snap.Bindings {
		switch b.Mode {
		case "direct":
			views[uid] = proxyView{ProxyTag: "直连"}
		case "fixed":
			if tag, ok := tags[b.ProxyID]; ok {
				views[uid] = proxyView{ProxyTag: tag, ProxyOK: enabled[b.ProxyID], ProxyID: b.ProxyID}
			} else {
				views[uid] = proxyView{ProxyTag: "节点已删除"}
			}
		}
	}
	return views
}

// overviewAccounts 账号池视图 + 代理注入 + 今日实测扣费积分注入。
func (p *Panel) overviewAccounts() []accountOverview {
	list := p.cfg.Pool.List()
	views := map[string]proxyView{}
	var snap proxypool.Snapshot
	var tags map[string]string
	var enabled map[string]bool
	if p.cfg.Proxypool != nil {
		snap = p.cfg.Proxypool.Snapshot()
		views = p.accountProxyViews(snap)
		tags, enabled = p.proxyNodeTags(snap)
	}
	var credits map[string]usage.DayCredits
	if p.cfg.Usage != nil {
		credits = p.cfg.Usage.TodayCreditsByUID(time.Now())
	}
	out := make([]accountOverview, len(list))
	for i, st := range list {
		item := accountOverview{Status: st}
		// proxy_id 的唯一事实来源是代理池 bindings；state.json 里的 legacy
		// proxy_id 只在启动迁移时读一次，不能当运行时状态透出去。
		item.ProxyID = ""
		if v, ok := views[st.UID]; ok {
			item.ProxyTag = v.ProxyTag
			item.ProxyOK = v.ProxyOK
			item.ProxyID = v.ProxyID
		}
		// 没有账号级绑定 → 按 realm 默认出口显示（issue #39 P3）。不补这一段，
		// 「分区默认 = 直连」的账号在面板上依旧写「随机」，又是一次文案骗人。
		if item.ProxyTag == "" {
			if rb, ok := snap.Realms[item.Realm]; ok {
				switch rb.Mode {
				case "direct":
					item.ProxyTag, item.ProxyOK, item.ProxyRealm = "直连", true, item.Realm
				case "fixed":
					if tag, ok := tags[rb.ProxyID]; ok {
						item.ProxyTag, item.ProxyOK = tag, enabled[rb.ProxyID]
						item.ProxyRealm = item.Realm
					}
				}
			}
		}
		if p.cfg.Usage != nil {
			if dc, ok := credits[st.UID]; ok {
				if dc.Observed {
					v := dc.Credits
					item.CreditsUsedToday = &v
				}
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
	// ProxyRealm 非空表示这行的出口来自 realm 默认（账号本身没绑定）：前端加「默认」角标。
	ProxyRealm       string   `json:"proxy_realm,omitempty"`
	CreditsUsedToday *float64 `json:"credits_used_today,omitempty"`
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
