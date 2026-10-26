// sync.go 订阅同步与手动导入。
//
// 同步语义（架构定案）：解析出的节点按 tag 去重后**整体替换**该订阅的旧节点——
// 机场侧删掉的节点本地消失；**只有真正消失的节点 ID** 才经回调解绑其上账号
// （ID 未变的节点继续保留绑定：绑定意图是「这个节点」，不是「这一轮解析结果」）；
// ID 变更（同名 tag 因池内重名后缀搬迁而重新键控）同样按消失处理，不留悬空绑定。
// 订阅只在同步时按需拉取，没有全局轮询任务。
package proxypool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Sync 拉取订阅并整体替换其节点。返回导入台账；订阅不存在返回错误
// （ErrSubNotFound 可 errors.Is 识别，供 panel 映射 404 而非 502）。
func (p *Pool) Sync(ctx context.Context, subID string) (SyncResult, error) {
	p.mu.Lock()
	sub, ok := p.subs[subID]
	if !ok {
		p.mu.Unlock()
		return SyncResult{}, fmt.Errorf("%w: %s", ErrSubNotFound, subID)
	}
	url := sub.URL
	p.mu.Unlock()

	body, err := p.fetch(ctx, url)
	if err != nil {
		return SyncResult{}, err
	}
	return p.importNodes(ctx, string(body), subID)
}

// ErrSubNotFound 订阅不存在（Sync/DeleteSubscription 共用；调用方按 404 映射）。
var ErrSubNotFound = errors.New("subscription not found")

// importNodes 解析正文并入池：按 tag 去重（池内唯一，重名加 -2 后缀）装配新节点集合，
// 整体替换该订阅的旧节点，并只对「真正消失的 ID」回调解绑。
//
// 两道守卫（都在改动任何状态之前）：
//   - 一个节点都解析不出 → 同步失败（机场错发登录页/限流页的典型形态）；
//   - 解析出了但全部校验失败且该订阅原有节点 → 同步失败（拒绝用空集替换，
//     否则一次坏响应就把该订阅整片清空，且 LastSyncAt 还会被记成成功）。
func (p *Pool) importNodes(_ context.Context, text, subID string) (SyncResult, error) {
	res := SyncResult{}
	maps, err := ParseNodes(text)
	if err != nil {
		return res, err
	}
	if len(maps) == 0 {
		return res, errors.New("no nodes parsed from subscription (html error page?)")
	}
	res.Total = len(maps)

	p.mu.Lock()
	sub, ok := p.subs[subID]
	if !ok {
		p.mu.Unlock()
		return res, fmt.Errorf("%w: %s", ErrSubNotFound, subID)
	}

	// 1. 旧节点快照：ID 集合 + 运维状态（Enabled/延迟观测）继承源。
	old := make(map[string]*Node, len(p.nodes))
	for id, n := range p.nodes {
		if n.SubID == subID {
			old[id] = n
		}
	}

	// 2. tag 去重键：池内现存 tag（含手动节点与其他订阅节点），但**排除本订阅的
	//    旧节点**——它们即将被替换，计入会让每次重同步把自己挤成 "-2" 后缀，
	//    ID 随之漂移（进而误解绑）。这正是「整体替换」与「差集解绑」的耦合点。
	taken := make(map[string]bool, len(p.nodes)+len(maps))
	for _, n := range p.nodes {
		if n.SubID == subID {
			continue
		}
		taken[n.Tag] = true
	}
	// 3. 校验 + 装配（尚未写入 p.nodes，失败可整体放弃）。
	nodes, errs := validateNodes(maps, subID, taken)
	if len(nodes) == 0 {
		p.mu.Unlock()
		// 台账照实回填（调用方与日志要能看出「解析到 N 个、全部无效」）。
		res.Skipped = res.Total
		res.Errors = errs
		// 解析出了节点但一个都活不下来 → 拒绝替换（旧节点与 LastSyncAt 原样保留）。
		// 空结果整体替换的后果：该订阅整片清空 + 其上账号全部解绑，而 LastSyncAt
		// 还会被记成「同步成功」，运维看到的是一片安静的空列表。
		if len(errs) > 0 {
			log.Printf("proxypool: subscription %s sync aborted, all %d parsed node(s) invalid: %s",
				subID, res.Total, clip(errs[0], 200))
		}
		if len(old) > 0 {
			return res, fmt.Errorf("subscription %s: all %d parsed node(s) failed validation, keeping existing %d node(s)",
				subID, res.Total, len(old))
		}
		return res, fmt.Errorf("subscription %s: all %d parsed node(s) failed validation", subID, res.Total)
	}

	// 4. 逐节点写入：沿用旧 ID 的节点继承运维状态（手动停用的坏节点不该被同步
	//    重新启用；延迟观测同理，否则面板每次同步都退回「尚未测速」）。
	//    内容真的变了（同 tag 换服务器/密码）才失效 transport 缓存——ID 复现但
	//    内容未变的节点保持原 transport 实例（同步不该无谓清空连接池）。
	newIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if prev, ok := old[n.ID]; ok {
			n.Enabled = prev.Enabled
			n.LatencyMS = prev.LatencyMS
			n.CheckedAt = prev.CheckedAt
			n.Fails = prev.Fails
			if !rawEqual(prev.Raw, n.Raw) {
				p.invalidateLocked(n.ID)
			}
		}
		p.nodes[n.ID] = n
		newIDs[n.ID] = true
	}

	// 5. 真正消失的节点 = 旧 ID − 新 ID：删除并解绑（差集为空则完全不动绑定）。
	var removed []string
	for id := range old {
		if !newIDs[id] {
			removed = append(removed, id)
			delete(p.nodes, id)
			p.invalidateLocked(id)
		}
	}

	res.Imported = len(nodes)
	res.Skipped = res.Total - res.Imported
	res.Errors = errs
	sub.LastSyncAt = time.Now().Unix()
	p.dirty.Store(true)
	p.mu.Unlock()

	// 6. 只对消失的节点解绑其账号（回调在锁外；nil 回调安全跳过）。
	if len(removed) > 0 {
		if uids := p.notifyUnbind(removed); len(uids) > 0 {
			log.Printf("proxypool: subscription %s resync removed %d node(s), unbound %d account(s)", subID, len(removed), len(uids))
		}
	}
	log.Printf("proxypool: subscription %s synced: total=%d imported=%d skipped=%d removed=%d errors=%d",
		subID, res.Total, res.Imported, res.Skipped, len(removed), len(res.Errors))
	return res, nil
}

// AddRaw 手动导入：多行 URI 或 Clash YAML 节点段，sub_id = ""（不随订阅同步）。
// 已存在的 tag 加后缀，不做整体替换。单次节点数上限 maxImportNodes——该接口与
// 网关同进程，一次粘贴十万节点足以打爆内存。
func (p *Pool) AddRaw(text string) (SyncResult, error) {
	res := SyncResult{}
	maps, err := ParseNodes(text)
	if err != nil {
		return res, err
	}
	res.Total = len(maps)
	if len(maps) == 0 {
		return res, errors.New("no nodes parsed from text")
	}
	// 超限部分按 skipped 计（不静默丢弃：errors 里给出明确原因）。
	overflow := 0
	if len(maps) > maxImportNodes {
		overflow = len(maps) - maxImportNodes
		maps = maps[:maxImportNodes]
	}

	p.mu.Lock()
	taken := make(map[string]bool, len(p.nodes)+len(maps))
	for _, n := range p.nodes {
		taken[n.Tag] = true
	}
	nodes, errs := validateNodes(maps, "", taken)
	for _, n := range nodes {
		p.nodes[n.ID] = n
	}
	res.Imported = len(nodes)
	res.Skipped = res.Total - res.Imported
	res.Errors = errs
	if overflow > 0 {
		res.Errors = append(res.Errors, fmt.Sprintf("node limit reached: %d node(s) beyond %d were not imported", overflow, maxImportNodes))
	}
	p.dirty.Store(true)
	p.mu.Unlock()
	return res, nil
}

// maxImportNodes 单次手动导入的节点数上限。
const maxImportNodes = 1000

// fetch 拉订阅正文：UA 按 clash-verge（机场按 UA 下发 Clash YAML），30s 超时，
// 非 2xx 视为失败（body 片段进错误信息便于排查机场返回的 HTML 错误页）。
func (p *Pool) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("bad subscription url: %w", err)
	}
	req.Header.Set("User-Agent", syncUA)
	resp, err := p.syncClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, errors.New("subscription http " + resp.Status + ": " + clip(string(raw), 200))
	}
	return raw, nil
}

// clip 截断长文本（错误信息防刷屏）。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
