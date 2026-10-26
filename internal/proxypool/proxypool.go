// Package proxypool 出站代理池：账号级出口 IP 管理。
//
// 职责（架构定案）：
//   - 节点/订阅的持久化（proxies.json，safefile 原子写 + 5s 防抖落盘，与 pool state.json
//     同模式：变更置 dirty，flusher 周期写，Close/Flush 兜底）；
//   - 订阅导入（Clash YAML proxies: 段 / base64 URI 列表；解析用 mihomo 库的
//     convert.ConvertsV2Ray + adapter.ParseProxy，见 parse.go）；
//   - 按节点构造 *http.Transport（DialContext 走节点拨号，域名由节点远端解析），
//     供 upstream.SetAccountTransport 按账号分流；同一节点被多个账号共用 = 同一出口 IP；
//   - 节点延迟测试（URLTest http://www.gstatic.com/generate_204，8s 超时）。
//
// 分层硬约束：不 import internal/pool / internal/upstream——transport 底座
// （upstream.NewTransport）与「删除节点后解绑账号」（pool.ClearProxyIfMatch）都由
// main 以闭包注入；panel 可同时 import 本包与 pool 做视图合并。
package proxypool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// 测试 URL 与超时先硬编码（config 不加新键；如需可配再进 config）。
// latencyTestURL 与 clash-speedtest 默认同款 204 探测地址；var 仅供测试注入
// 本地 httptest 地址（生产恒 gstatic，同 wafIPWindow 的测试注入先例）。
var (
	latencyTestURL = "http://www.gstatic.com/generate_204"
	latencyTimeout = 8 * time.Second
)

// syncTimeout 订阅拉取上限；syncUA 机场订阅端按 UA 下发对应格式
// （clash-verge 拿到的是 Clash YAML）。testConcurrency 批量测延迟并发上限。
const (
	syncTimeout     = 30 * time.Second
	syncUA          = "clash-verge/v2.0.0"
	testConcurrency = 8
)

// flushInterval 后台落盘周期（与 pool flushInterval 同节奏）。
const flushInterval = 5 * time.Second

// Subscription 机场订阅。ID = sha256(URL) 前 8 位（内容寻址、重启稳定；同一 URL
// 重复创建幂等归并）。LastSyncAt unix 秒，0 = 从未同步成功过。
type Subscription struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	LastSyncAt int64  `json:"last_sync_at"`
}

// Node 代理节点。ID = sha256(tag) 前 10 位——tag（节点名）是池内唯一键，重名加
// "-2" 后缀，故 ID 随 tag 稳定。SubID = 来源订阅（手动添加为 ""）。
// Raw 是完整 clash proxy map（mihomo adapter 可直接 ParseProxy），JSON 序列化落盘。
type Node struct {
	ID     string         `json:"id"`
	Tag    string         `json:"tag"`
	Type   string         `json:"type"` // ss/vmess/trojan/hysteria2/vless/http/socks5...
	Server string         `json:"server"`
	Port   int            `json:"port"`
	SubID  string         `json:"sub_id"`
	Raw    map[string]any `json:"raw"`
	// Enabled 默认 true；停用后 TransportFor 返回 nil（该节点绑定的账号回落直连）。
	Enabled bool `json:"enabled"`
	// LatencyMS 最近一次成功测得的延迟（毫秒；0 = 无有效观测）。CheckedAt unix 秒。
	// Fails 连续失败计数（成功清零），供面板标注「最近不通」。
	LatencyMS int   `json:"latency_ms"`
	CheckedAt int64 `json:"checked_at"`
	Fails     int   `json:"fails"`
}

// TestResult 单节点延迟测试结果（TestBatch 输出，顺序与入参一致）。
type TestResult struct {
	ID        string `json:"id"`
	OK        bool   `json:"ok"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// NodeView 节点存在性/状态摘要（panel overview 与 bind_proxy 回显用）。
type NodeView struct {
	ID      string `json:"id"`
	Tag     string `json:"tag"`
	Enabled bool   `json:"enabled"`
}

// SubView 订阅 + 其节点数（面板列表用）。
type SubView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	LastSyncAt int64  `json:"last_sync_at"`
	Count      int    `json:"count"`
}

// SyncResult 一次导入/同步的台账：total = 解析出的节点数；imported = 实际入池数
// （含整体替换重导入）；skipped = 解析到但无效被丢弃数；errors = 逐节点失败原因。
type SyncResult struct {
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// diskFile 落盘格式（version 便于将来迁移）。
type diskFile struct {
	Version int                      `json:"version"`
	Saved   string                   `json:"saved,omitempty"`
	Subs    map[string]*Subscription `json:"subs"`
	Nodes   map[string]*Node         `json:"nodes"`
}

// diskNode 落盘节点的读取中间态：Enabled 用指针承接，区分「字段缺失」与「显式 false」。
// Node 本体不能直接反序列化——enabled 缺失会落成零值 false，把手工编辑/半截写入的
// 节点静默判为停用（该字段语义是「缺省启用」）。写入仍走 Node（实体标量 bool），
// 落盘格式逐字节不变。
type diskNode struct {
	ID        string         `json:"id"`
	Tag       string         `json:"tag"`
	Type      string         `json:"type"`
	Server    string         `json:"server"`
	Port      int            `json:"port"`
	SubID     string         `json:"sub_id"`
	Raw       map[string]any `json:"raw"`
	Enabled   *bool          `json:"enabled"`
	LatencyMS int            `json:"latency_ms"`
	CheckedAt int64          `json:"checked_at"`
	Fails     int            `json:"fails"`
}

// diskFileLoad 读盘形状：nodes 走 diskNode（pointer 元素，nil 条目即 `"abc":null`），
// subs 同理——两者都会让 List/Subs 的解引用崩在运行期，必须在 load 期挡掉。
type diskFileLoad struct {
	Version int                      `json:"version"`
	Saved   string                   `json:"saved,omitempty"`
	Subs    map[string]*Subscription `json:"subs"`
	Nodes   map[string]*diskNode     `json:"nodes"`
}

const fileVersion = 1

// Pool 代理池：节点与订阅的内存态 + 持久化 + transport 缓存。并发安全。
type Pool struct {
	fp   string                 // proxies.json 路径（main 由 stateSibling 推出）
	base func() *http.Transport // transport 底座构造器（upstream.NewTransport 注入）

	mu    sync.Mutex
	subs  map[string]*Subscription
	nodes map[string]*Node
	dirty atomic.Bool
	// trCache node id → 已构造的 transport。所有读写都持有 p.mu（构造含 ParseProxy
	// 与 map 变更同一临界区，杜绝「更新失效 vs 构建回填」竞态）；调用频率是
	// clientFor 缓存未命中级（每节点一次），锁竞争可忽略。
	trCache map[string]*http.Transport

	// stopCh 后台落盘退出信号；closeOnce 保证 Close 幂等（并发 Close 不 double-close，
	// 与 internal/pool 的 closeOnce 同风格）。
	stopCh    chan struct{}
	closeOnce sync.Once

	// dialTimeout 节点建连上限（main 用 SetNodeDialTimeout(upstream.DialTimeout) 注入，
	// 让「与共享 transport 同参数」是同一常量）；0 → DefaultNodeDialTimeout。
	dialTimeout time.Duration

	// unbind 节点删除回调（main 注入：pool.ClearProxyIfMatch）。在 p.mu 外调用。
	unbind func(ids []string) []string

	syncClient *http.Client // 订阅拉取专用（30s 超时）
}

// New 构建池：加载旧状态并启动后台防抖落盘。fp 为空 = 纯内存（测试用）；
// base 为空时 TransportFor 恒 nil（调用方装配缺失，行为退化为直连）。
func New(fp string, base func() *http.Transport) *Pool {
	p := &Pool{
		fp:         fp,
		base:       base,
		subs:       map[string]*Subscription{},
		nodes:      map[string]*Node{},
		trCache:    map[string]*http.Transport{},
		syncClient: &http.Client{Timeout: syncTimeout},
	}
	p.load()
	p.startFlusher()
	return p
}

// SetUnbindCallback 注入节点删除回调：ids 为被删节点集合，返回被解绑的 uid 列表
// （仅用于返回给调用方汇总）。在 p.mu 外同步调用。
func (p *Pool) SetUnbindCallback(fn func(ids []string) []string) { p.unbind = fn }

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	if p.fp == "" {
		return
	}
	raw, err := safefile.Read(p.fp, nil)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("proxypool: proxies.json 不可用（%v），跳过恢复: %s", err, p.fp)
		}
		return
	}
	var df diskFileLoad
	if err := json.Unmarshal(raw, &df); err != nil {
		log.Printf("proxypool: proxies.json 解析失败（%v），原文件保留待人工处理: %s", err, p.fp)
		return
	}
	if df.Subs != nil {
		subs := make(map[string]*Subscription, len(df.Subs))
		for id, s := range df.Subs {
			if s == nil {
				log.Printf("proxypool: proxies.json 订阅 %q 为 null，已丢弃", id)
				continue
			}
			s.ID = id // ID 是主键：以 map 键为准，避免条目内 id 字段与键漂移
			subs[id] = s
		}
		p.subs = subs
	}
	if df.Nodes != nil {
		p.nodes = p.sanitizeNodesLocked(df.Nodes)
	}
}

// sanitizeNodesLocked 过滤读盘节点：nil 条目（JSON null）、ID 与 tagID(tag) 不一致的
// 脏条目一律丢弃并记日志（否则 List/TransportFor 会在运行期 nil 解引用）；缺 enabled
// 字段按「缺省启用」补齐（Node.Enabled 的语义是默认 true）。调用方持 p.mu 或处在
// 构造期（此时尚无并发）。
func (p *Pool) sanitizeNodesLocked(loaded map[string]*diskNode) map[string]*Node {
	out := make(map[string]*Node, len(loaded))
	for id, dn := range loaded {
		if dn == nil {
			log.Printf("proxypool: proxies.json 节点 %q 为 null，已丢弃", id)
			continue
		}
		if want := tagID(dn.Tag); want != id {
			// ID 派生自 tag，两者不一致说明条目被外部改坏（手改/半截合并）：
			// 保留会让「按 id 绑定的账号」指到一个永不再匹配的键上。
			log.Printf("proxypool: proxies.json 节点 %q 的 id 与 tag %q 不匹配（want %s），已丢弃", id, dn.Tag, want)
			continue
		}
		n := &Node{
			ID:        id,
			Tag:       dn.Tag,
			Type:      dn.Type,
			Server:    dn.Server,
			Port:      dn.Port,
			SubID:     dn.SubID,
			Raw:       dn.Raw,
			LatencyMS: dn.LatencyMS,
			CheckedAt: dn.CheckedAt,
			Fails:     dn.Fails,
		}
		// 缺省启用：字段缺失 → true；显式 false → 保持停用。
		n.Enabled = dn.Enabled == nil || *dn.Enabled
		out[id] = n
	}
	return out
}

// startFlusher 周期防抖落盘（与 pool.startFlusher 同模式：失败把 dirty 置回重试）。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	p.stopCh = stopCh
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				if p.dirty.Swap(false) && !p.saveLocked() {
					p.dirty.Store(true)
				}
				p.mu.Unlock()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步落盘（幂等：无变更不写盘）。进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) && !p.saveLocked() {
		p.dirty.Store(true)
	}
	p.mu.Unlock()
}

// Close 停止后台落盘并做最后一次 Flush。幂等：closeOnce 保证并发/重复调用不
// double-close（与 internal/pool 的 Close 同风格）。
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		if p.stopCh != nil {
			close(p.stopCh)
		}
	})
	p.Flush()
}

// saveLocked 落盘（调用方须持 p.mu）。safefile：fsync 后 rename + .bak 兜底。
func (p *Pool) saveLocked() bool {
	if p.fp == "" {
		return true
	}
	df := diskFile{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Subs: p.subs, Nodes: p.nodes}
	raw, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		log.Printf("proxypool: proxies.json 序列化失败: %v", err)
		return false
	}
	if dir := filepath.Dir(p.fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := safefile.WriteKeep(p.fp, raw, 0o600); err != nil {
		log.Printf("proxypool: proxies.json 落盘失败: %v", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 查询视图
// ---------------------------------------------------------------------------

// List 返回全部节点（按 tag 升序，输出稳定）。
func (p *Pool) List() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// Subs 返回全部订阅及各自节点数（按 id 升序，输出稳定）。
func (p *Pool) Subs() []SubView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SubView, 0, len(p.subs))
	for _, s := range p.subs {
		v := SubView{ID: s.ID, Name: s.Name, URL: s.URL, LastSyncAt: s.LastSyncAt}
		for _, n := range p.nodes {
			if n.SubID == s.ID {
				v.Count++
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NodeView 返回节点摘要；不存在返回 ok=false。
func (p *Pool) NodeView(id string) (NodeView, bool) {
	if id == "" {
		return NodeView{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return NodeView{}, false
	}
	return NodeView{ID: n.ID, Tag: n.Tag, Enabled: n.Enabled}, true
}

// ExitKey 返回某绑定意图的**实际出口**标识：id 空 / 节点不存在 / 节点已停用 →
// ""（直连组）。即「TransportFor 返回非 nil 的充要条件」，供按出口分组的消费者
// （WAF IP 级拦截）与 TransportFor 保持同一口径——按绑定意图分组会把「实际走直连
// 的账号」拆成各自独立的小组，稀释直连组的判定。
//
// 与 TransportFor 的唯一差别：节点 Raw 解析失败时 TransportFor 也回落直连，但那
// 是脏数据兜底（入池已校验），且每次解析有成本；这里不再 ParseProxy。
func (p *Pool) ExitKey(id string) string {
	if id == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok || !n.Enabled {
		return ""
	}
	return id
}

// ---------------------------------------------------------------------------
// 变更操作
// ---------------------------------------------------------------------------

// AddSubscription 新建订阅（幂等：同 URL 返回既有订阅）。不自动同步，同步走 Sync。
// URL 立即校验 scheme（仅 http/https）：否则一个 ftp:// 或裸主机名的订阅会先入池、
// 直到 Sync 才在 http.NewRequest 处失败，且留下一个永远同步不上的订阅条目。
func (p *Pool) AddSubscription(name, url string) (*Subscription, error) {
	url = trimSpace(url)
	if url == "" {
		return nil, errors.New("url is required")
	}
	if err := validateSubURL(url); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.subs[subIDFor(url)]; ok {
		return s, nil
	}
	s := &Subscription{ID: subIDFor(url), Name: trimSpace(name), URL: url}
	p.subs[s.ID] = s
	p.dirty.Store(true)
	return s, nil
}

// validateSubURL 订阅地址校验：必须是带主机名的 http/https URL。
func validateSubURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid subscription url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("invalid subscription url scheme %q (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("invalid subscription url: missing host")
	}
	return nil
}

// DeleteSubscription 删除订阅及其全部节点；经回调解绑被删节点的账号。
// 返回被删节点 id 与被解绑 uid（均可能为 nil）。
func (p *Pool) DeleteSubscription(id string) (removedIDs, unbound []string, err error) {
	p.mu.Lock()
	if _, ok := p.subs[id]; !ok {
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrSubNotFound, id)
	}
	delete(p.subs, id)
	for nid, n := range p.nodes {
		if n.SubID == id {
			removedIDs = append(removedIDs, nid)
		}
	}
	p.dropNodesLocked(removedIDs)
	p.dirty.Store(true)
	p.mu.Unlock()
	if len(removedIDs) > 0 {
		unbound = p.notifyUnbind(removedIDs)
	}
	return removedIDs, unbound, nil
}

// Delete 删除单节点（订阅同步的整体替换不走这里）；经回调解绑其账号。
func (p *Pool) Delete(id string) (unbound []string, err error) {
	p.mu.Lock()
	if _, ok := p.nodes[id]; !ok {
		p.mu.Unlock()
		return nil, fmt.Errorf("proxy %s not found", id)
	}
	delete(p.nodes, id)
	p.dropNodesLocked([]string{id})
	p.dirty.Store(true)
	p.mu.Unlock()
	unbound = p.notifyUnbind([]string{id})
	return unbound, nil
}

// Toggle 启用/停用节点（取反），返回变更后的 Enabled。停用节点上的在途请求不受
// 影响（连接已建立）；新请求 TransportFor 返回 nil → 直连。
func (p *Pool) Toggle(id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return false, fmt.Errorf("proxy %s not found", id)
	}
	n.Enabled = !n.Enabled
	// 状态翻转即失效缓存：停用后 transport 不许再被复用，启用后按新状态重建。
	p.invalidateLocked(id)
	p.dirty.Store(true)
	return n.Enabled, nil
}

// dropNodesLocked 删除节点并失效其 transport（调用方须持 p.mu）。
func (p *Pool) dropNodesLocked(ids []string) {
	for _, id := range ids {
		delete(p.nodes, id)
		p.invalidateLocked(id)
	}
}

// invalidateLocked 失效并清空某节点的 transport 缓存（关闭空闲连接；在途请求不受影响）。
func (p *Pool) invalidateLocked(id string) {
	tr, ok := p.trCache[id]
	if !ok {
		return
	}
	delete(p.trCache, id)
	if tr != nil {
		go tr.CloseIdleConnections()
	}
}

// notifyUnbind 在锁外调用解绑回调（nil = 无回调，返回 nil）。
func (p *Pool) notifyUnbind(ids []string) []string {
	if p.unbind == nil || len(ids) == 0 {
		return nil
	}
	return p.unbind(ids)
}

// tagID 节点稳定标识：sha256(tag)[:10]（hex）。
func tagID(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return hex.EncodeToString(sum[:])[:10]
}

// subIDFor 订阅稳定标识：sha256(url)[:8]（hex）。
func subIDFor(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])[:8]
}

// allocTag 在 taken（池内已占用 tag 集合）中为 tag 分配唯一名：重名加 -2/-3 后缀。
func allocTag(tag string, taken map[string]bool) string {
	if tag == "" {
		tag = "unnamed"
	}
	if !taken[tag] {
		taken[tag] = true
		return tag
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", tag, i)
		if !taken[cand] {
			taken[cand] = true
			return cand
		}
	}
}

// buildNode 从 clash proxy map 装配节点（含类型/服务器/端口归一化）。
// 校验失败的 map 返回 error（调用方记 skipped+errors）。
func buildNode(raw map[string]any, tag, subID string) (*Node, error) {
	typ, _ := raw["type"].(string)
	server, _ := raw["server"].(string)
	port := mapInt(raw, "port")
	if typ == "" || server == "" || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid proxy map (type=%q server=%q port=%v)", typ, server, raw["port"])
	}
	return &Node{
		ID:      tagID(tag),
		Tag:     tag,
		Type:    typ,
		Server:  server,
		Port:    port,
		SubID:   subID,
		Raw:     raw,
		Enabled: true,
	}, nil
}

// rawEqual 比较两份 clash proxy map 是否等价（同步时判断节点内容是否真的变了）。
// JSON 序列化的键序是确定的（encoding/json 对 map 键排序），故可直接比字节。
func rawEqual(a, b map[string]any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

// mapInt 从 clash map 取整型字段（兼容 yaml int / URI 转换的 string / json float64）。
func mapInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint16:
		return int(v)
	case float64:
		return int(v)
	case string:
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// trimSpace strings.TrimSpace 的本地别名（保持本文件 import 面干净）。
func trimSpace(s string) string { return strings.TrimSpace(s) }
