// bridge.go 机场订阅（vless/hysteria2/…）→ 本机 socks5 监听 → 上游 #122 代理池。
//
// 背景：上游 #122 的池只吃「一行一个代理 URL」（http/https/socks5/socks5h），而本
// fork 的节点来自 Clash 订阅（原生协议，无法表达成 URL）。本包做桥：给每个节点在
// 127.0.0.1 上开一个 socks5 监听，用 mihomo 出站适配器真实拨号，然后把
// `socks5://127.0.0.1:PORT` 交给上游 Manager —— 路由/健康检查/绑定全归上游。
//
// 端口确定性：端口由节点身份（type/server/port/name）哈希决定，同一节点重启后拿到
// 同一端口 → URL 不变 → 上游 Manager 认成同一节点，绑定与健康历史都不丢。
package nodebridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
)

// basePort 本地 socks5 监听起点（每节点一个，最多 spanPorts 个）。
const (
	basePort  = 39100
	spanPorts = 1200
)

// Sub 机场订阅。
type Sub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	LastSync int64  `json:"last_sync_at"`
}

// legacyNode 落盘节点（沿用本 fork 旧 proxies.json 的结构，备份可直接回滚）。
type legacyNode struct {
	ID        string         `json:"id"`
	Tag       string         `json:"tag"`
	Type      string         `json:"type"`
	Server    string         `json:"server"`
	Port      int            `json:"port"`
	SubID     string         `json:"sub_id"`
	Enabled   bool           `json:"enabled"`
	LatencyMS int            `json:"latency_ms"`
	CheckedAt int64          `json:"checked_at"`
	Fails     int            `json:"fails"`
	Raw       map[string]any `json:"raw"`
}

type legacyDisk struct {
	Version int `json:"version"`
	// Saved 旧版本写的是 RFC3339 字符串（"2026-11-01T16:24:14+08:00"），
	// 按 int64 解会让整个文件解析失败、55 个节点一个都载不进来——这里
	// 只当透传，不参与任何逻辑。
	Saved json.RawMessage        `json:"saved"`
	Subs  map[string]*Sub        `json:"subs"`
	Nodes map[string]*legacyNode `json:"nodes"`
	// History 本地 socks5 URL → 节点名的历史（订阅换了身份后，旧 URL 只剩这里能反查）。
	// 旧版本不认这个字段：Go 解未知字段是忽略，回滚只丢历史，不会解析失败。
	History map[string]histEntry `json:"history,omitempty"`
	// Ports 节点 ID → 本机 socks5 端口。重启后必须复用同一个，否则 URL 变、
	// 池里节点身份跟着变、绑定全部悬空（2026-11-02 就这么翻过一次车）。
	// 旧版本忽略这个字段，回滚只是回到「按身份哈希重新分配」。
	Ports map[string]int `json:"ports,omitempty"`
}

// histEntry 一条 URL→节点名的历史记录。
type histEntry struct {
	Tag string `json:"tag"`
	At  int64  `json:"at"`
}

// Meta 节点元信息（面板列表用）。
type Meta struct {
	ID     string
	Tag    string
	Type   string
	Server string
	Port   int
	SubID  string
	URL    string
}

type entry struct {
	legacyNode
	url   string
	proxy C.Proxy
	ln    net.Listener
	// lport 是本机 socks5 监听端口；legacyNode.Port 始终是节点的远端端口
	// （落盘字段不能写本地端口，否则重启后身份哈希变化、绑定全断）。
	lport int
}

// Bridge 订阅节点 → 本地 socks5 出口。
type Bridge struct {
	mu    sync.Mutex
	path  string
	subs  map[string]*Sub
	nodes map[string]*entry // node id → entry
	byURL map[string]*entry
	// hist 是 URL→节点名的历史：机场换一批节点后端口全变，绑定迁移只能靠
	// 「旧 URL 当初叫什么名字」找回同名新节点。
	hist map[string]histEntry
	// ports 上次实际用的本地端口（node id → port），重启按它复用。
	ports  map[string]int
	closed bool
	client *http.Client
}

// Load 读旧 proxies.json（不存在则返回空桥，不报错）。
func Load(path string) (*Bridge, error) {
	b := &Bridge{
		path:  path,
		subs:  map[string]*Sub{},
		nodes: map[string]*entry{},
		byURL: map[string]*entry{},
		hist:  map[string]histEntry{},
		ports: map[string]int{},
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        16,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	var disk legacyDisk
	if err := json.Unmarshal(raw, &disk); err != nil {
		return nil, fmt.Errorf("parse node file: %w", err)
	}
	for id, s := range disk.Subs {
		if s != nil {
			if s.ID == "" {
				s.ID = id
			}
			b.subs[id] = s
		}
	}
	for id, port := range disk.Ports {
		if port > 0 {
			b.ports[id] = port
		}
	}
	for u, h := range disk.History {
		if h.Tag != "" {
			b.hist[u] = h
		}
	}
	for id, n := range disk.Nodes {
		if n == nil || n.Raw == nil {
			continue
		}
		if infoOnlyTag(n.Tag) { // 机场套餐文案行，不是能用的出口
			continue
		}
		if n.ID == "" {
			n.ID = id
		}
		b.nodes[id] = &entry{legacyNode: *n}
	}
	return b, nil
}

// Start 为所有已启用节点打开本地 socks5 监听。
func (b *Bridge) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.nodes))
	for id := range b.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 端口分配的确定性前提
	used := map[int]bool{}
	for _, id := range ids {
		e := b.nodes[id]
		if !e.Enabled {
			continue
		}
		if err := b.startLocked(e, used); err != nil {
			log.Printf("nodebridge: node %s (%s) skipped: %v", e.ID, e.Tag, err)
			e.Enabled = false
			continue
		}
	}
	log.Printf("nodebridge: %d node(s) listening on 127.0.0.1:%d+", len(b.byURL), basePort)
	return nil
}

func (b *Bridge) startLocked(e *entry, used map[int]bool) error {
	if e.ln != nil {
		return nil
	}
	p, err := adapter.ParseProxy(e.Raw)
	if err != nil {
		return err
	}
	// 端口优先复用上次落盘的那个：URL 稳定 = 池里节点身份稳定 = 绑定不会因为一次
	// 重启就指向不存在的节点。哈希只作为首次分配/端口被占时的兜底。
	var ln net.Listener
	if pref := b.ports[e.ID]; pref >= basePort && pref < basePort+spanPorts && !used[pref] {
		if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pref))); err == nil {
			ln = l
			used[pref] = true
			e.lport = pref
		}
	}
	if ln == nil {
		// 首次分配按节点身份哈希：同一节点每次冷启动都拿到同一个端口。
		h := fnv.New32a()
		fmt.Fprintf(h, "%s|%s|%d|%s", e.Type, e.Server, e.Port, e.Tag)
		start := basePort + int(h.Sum32()%spanPorts)
		for i := 0; i < spanPorts; i++ {
			port := basePort + (start-basePort+i)%spanPorts
			if used[port] {
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				continue
			}
			ln = l
			used[port] = true
			e.lport = port
			break
		}
	}
	if ln == nil {
		return fmt.Errorf("no free local port for socks5 bridge")
	}
	e.proxy = p
	e.ln = ln
	e.url = "socks5://" + ln.Addr().String()
	e.Enabled = true
	b.byURL[e.url] = e
	b.ports[e.ID] = e.lport
	b.recordTagLocked(e.url, e.Tag)
	go b.serve(ln, p, e.Tag)
	return nil
}

// serve 极简 SOCKS5（无认证）服务端：握手 → CONNECT → 用 mihomo 出站拨号 → 双向转发。
func (b *Bridge) serve(ln net.Listener, p C.Proxy, tag string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go func(c net.Conn) {
			defer c.Close()
			if err := socks5Serve(c, p); err != nil {
				log.Printf("nodebridge: %s tunnel: %v", tag, err)
			}
		}(conn)
	}
}

func socks5Serve(c net.Conn, p C.Proxy) error {
	c.SetDeadline(time.Now().Add(20 * time.Second))
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 5 {
		return fmt.Errorf("unsupported socks version %d", hdr[0])
	}
	if _, err := io.ReadFull(c, make([]byte, int(hdr[1]))); err != nil {
		return err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil { // 无认证
		return err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return err
	}
	if req[0] != 5 || req[1] != 1 { // 只支持 CONNECT
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("unsupported socks command %d", req[1])
	}
	var host string
	md := &C.Metadata{NetWork: C.TCP}
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return err
		}
		host = net.IP(ip).String()
		if ip, e := netip.ParseAddr(host); e == nil {
			md.DstIP = ip.Unmap()
		}
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return err
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, name); err != nil {
			return err
		}
		host = string(name)
		md.Host = host
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return err
		}
		host = net.IP(ip).String()
		if ip, e := netip.ParseAddr(host); e == nil {
			md.DstIP = ip.Unmap()
		}
	default:
		return fmt.Errorf("unsupported address type %d", req[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(pb)
	md.DstPort = port
	c.SetDeadline(time.Time{})

	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	up, err := p.DialContext(dctx, md)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("dial %s:%d: %w", host, port, err)
	}
	defer up.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	return nil
}

// Lines 上游 Manager.Import 的入参：每行一个 socks5://127.0.0.1:port。
func (b *Bridge) Lines() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	urls := make([]string, 0, len(b.byURL))
	for u, e := range b.byURL {
		if e.Enabled {
			urls = append(urls, u)
		}
	}
	sort.Strings(urls)
	return strings.Join(urls, "\n")
}

// Meta 按上游池里的 URL 反查节点元信息（面板列表/绑定视图用）。
func (b *Bridge) Meta(u string) (Meta, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.byURL[u]
	if !ok {
		return Meta{}, false
	}
	return Meta{ID: e.ID, Tag: e.Tag, Type: e.Type, Server: e.Server, Port: e.Port, SubID: e.SubID, URL: e.url}, true
}

// URLOf 按旧节点 ID 取它的 socks5 URL（state.json 老 proxy_id 的迁移入口）。
func (b *Bridge) URLOf(id string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.nodes[id]
	if !ok || e.url == "" {
		return "", false
	}
	return e.url, true
}

// Keep 当前活节点的 URL 集合（代理池对账用：集合之外的池节点都是陈旧条目）。
func (b *Bridge) Keep() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]bool, len(b.byURL))
	for u, e := range b.byURL {
		if e.Enabled {
			out[u] = true
		}
	}
	return out
}

// TagForURL 节点名反查：活节点优先，其次查历史（订阅换身份后旧 URL 只剩历史）。
// 供 proxypool.Reconcile 把「绑定在已消失 URL 上的账号」迁到同名的新节点。
func (b *Bridge) TagForURL(u string) string {
	if u == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.byURL[u]; ok {
		return e.Tag
	}
	return b.hist[u].Tag
}

// recordTagLocked 记 URL→节点名（调用方持 b.mu）。上限 4000，超了按时间淘汰最旧
// 的一批 —— 真正需要的只有相邻两次换订阅之间那一小段历史。
func (b *Bridge) recordTagLocked(u, tag string) {
	if u == "" || tag == "" {
		return
	}
	if b.hist == nil {
		b.hist = map[string]histEntry{}
	}
	if old, ok := b.hist[u]; ok && old.Tag == tag {
		return
	}
	b.hist[u] = histEntry{Tag: tag, At: time.Now().Unix()}
	const maxHist = 4000
	if len(b.hist) <= maxHist {
		return
	}
	keys := make([]string, 0, len(b.hist))
	for k := range b.hist {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return b.hist[keys[i]].At < b.hist[keys[j]].At })
	for _, k := range keys[:len(keys)-maxHist+maxHist/10] {
		delete(b.hist, k)
	}
}

// Subs 订阅列表（按名称排序，输出稳定）。
func (b *Bridge) Subs() []Sub {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Sub, 0, len(b.subs))
	for _, s := range b.subs {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Sub 取单个订阅。
func (b *Bridge) Sub(id string) (Sub, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.subs[id]
	if !ok {
		return Sub{}, false
	}
	return *s, true
}

// AddSub 新增订阅并立即拉一次。
func (b *Bridge) AddSub(ctx context.Context, name, url string) (Sub, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return Sub{}, fmt.Errorf("订阅地址必须是 http(s) URL")
	}
	id := subIDFor(url)
	b.mu.Lock()
	if _, ok := b.subs[id]; ok {
		b.mu.Unlock()
		return Sub{}, fmt.Errorf("该订阅已存在")
	}
	s := &Sub{ID: id, Name: name, URL: url}
	b.subs[id] = s
	b.mu.Unlock()
	if _, err := b.Sync(ctx, id); err != nil {
		return Sub{}, err
	}
	return *s, nil
}

// DelSub 删除订阅及其节点（先关闭监听）。
func (b *Bridge) DelSub(id string) error {
	b.mu.Lock()
	if _, ok := b.subs[id]; !ok {
		b.mu.Unlock()
		return fmt.Errorf("订阅不存在: %s", id)
	}
	delete(b.subs, id)
	var drop []*entry
	for nid, e := range b.nodes {
		if e.SubID == id {
			drop = append(drop, e)
			delete(b.nodes, nid)
			delete(b.byURL, e.url)
		}
	}
	b.mu.Unlock()
	for _, e := range drop {
		if e.ln != nil {
			e.ln.Close()
		}
	}
	b.save()
	return nil
}

// Sync 重新拉取订阅并把节点集合替换为该次解析结果（同 tag 保留启用状态）。
func (b *Bridge) Sync(ctx context.Context, id string) (int, error) {
	b.mu.Lock()
	s, ok := b.subs[id]
	b.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("订阅不存在: %s", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "clash.meta")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("拉取订阅失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	maps, err := ParseNodes(string(body))
	if err != nil {
		return 0, err
	}
	if len(maps) == 0 {
		return 0, fmt.Errorf("订阅里没有解析到节点（机场返回错误页？）")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	added := 0
	for _, m := range maps {
		tag, _ := m["name"].(string)
		typ, _ := m["type"].(string)
		server, _ := m["server"].(string)
		port := mapInt(m["port"])
		if strings.TrimSpace(tag) == "" || typ == "" || server == "" || port <= 0 {
			continue
		}
		if infoOnlyTag(tag) { // 「剩余流量：1.89 TB」这类信息行永远探不通
			continue
		}
		nid := nodeIDFor(typ, server, port, tag)
		seen[nid] = true
		if e, ok := b.nodes[nid]; ok {
			e.Raw = m
			continue
		}
		e := &entry{legacyNode: legacyNode{
			ID: nid, Tag: tag, Type: typ, Server: server, Port: port,
			SubID: id, Enabled: true, Raw: m,
		}}
		b.nodes[nid] = e
		added++
	}
	// 消失的节点：关监听、移出
	var drop []*entry
	for nid, e := range b.nodes {
		if e.SubID != id || seen[nid] {
			continue
		}
		drop = append(drop, e)
		delete(b.nodes, nid)
		delete(b.byURL, e.url)
		delete(b.ports, nid)
	}
	// 先关旧监听再开新监听：否则新节点算出的端口会被「还没关的旧监听」占着，
	// net.Listen 失败后退到下一位，整批端口漂移 —— 池里旧 URL 全成僵尸的元凶。
	for _, e := range drop {
		if e.ln != nil {
			e.ln.Close()
			e.ln = nil
		}
	}
	used := map[int]bool{}
	for _, e := range b.byURL {
		if e.lport > 0 {
			used[e.lport] = true
		}
	}
	newIDs := make([]string, 0, len(b.nodes))
	for nid, e := range b.nodes {
		if e.SubID == id && e.ln == nil && e.Enabled {
			newIDs = append(newIDs, nid)
		}
	}
	sort.Strings(newIDs) // 端口分配的确定性前提（与 Start 同一顺序）
	for _, nid := range newIDs {
		if err := b.startLocked(b.nodes[nid], used); err != nil {
			log.Printf("nodebridge: restart node %s failed: %v", b.nodes[nid].Tag, err)
		}
	}
	s.LastSync = time.Now().Unix()
	go b.saveAsync()
	log.Printf("nodebridge: subscription %s synced: total=%d added=%d removed=%d", id, len(maps), added, len(drop))
	return added, nil
}

func (b *Bridge) saveAsync() {
	b.save()
}

// save 把节点集合写回旧格式文件（回滚时可直接给旧版本用）。
func (b *Bridge) save() {
	b.mu.Lock()
	// 与旧版本完全同形：saved 写 RFC3339 字符串（旧版本按 string 读，回滚不炸）。
	stamp, _ := json.Marshal(time.Now().Format(time.RFC3339))
	disk := legacyDisk{Version: 1, Saved: stamp, Subs: b.subs, Nodes: map[string]*legacyNode{}}
	for id, e := range b.nodes {
		n := e.legacyNode
		disk.Nodes[id] = &n
	}
	hist := make(map[string]histEntry, len(b.hist))
	for u, h := range b.hist {
		hist[u] = h
	}
	disk.History = hist
	ports := make(map[string]int, len(b.ports))
	for id, port := range b.ports {
		ports[id] = port
	}
	disk.Ports = ports
	path := b.path
	b.mu.Unlock()
	raw, err := json.MarshalIndent(disk, "", " ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("nodebridge: save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("nodebridge: save rename failed: %v", err)
	}
}

// Close 关闭所有本地监听。
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, e := range b.nodes {
		if e.ln != nil {
			e.ln.Close()
			e.ln = nil
		}
	}
}

func mapInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

func nodeIDFor(typ, server string, port int, tag string) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%d|%s", typ, server, port, tag)
	return fmt.Sprintf("%012x", h.Sum64())[:12]
}

func subIDFor(url string) string {
	h := fnv.New64a()
	fmt.Fprint(h, url)
	return fmt.Sprintf("%012x", h.Sum64())[:8]
}
