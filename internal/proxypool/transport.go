// transport.go 节点 → *http.Transport：把 mihomo 出站适配器的拨号能力包装成
// 标准库 DialContext，塞进以注入底座（upstream.NewTransport）为基础的 transport。
//
// 关键口径（保持既有 DNS 行为）：
//   - addr 是 host:port。host 是 IP → 填 Metadata.DstIP；是域名 → 填 Metadata.Host，
//     解析交给节点远端（服务器侧代解析）——不走本地 SetDNSDialer，也不在本进程
//     预解析，防止本地污染 DNS 影响出口选择。
//   - transport 以 base() 为底：连接层加固（禁 h2 / TLS 握手超时 / keepalive 探测）
//     与共享 transport 同参数；仅替换 DialContext。
//   - 底座的 net.Dialer.Timeout 在 DialContext 被整体替换后完全失效：mihomo 出站
//     自建 dialer 且不设 Timeout，建连上限只能由本层用 context 兜住（见
//     DefaultNodeDialTimeout 的注释）。
package proxypool

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
)

// DefaultNodeDialTimeout 节点单次建连上限的默认值（10s，与 upstream 共享 transport
// 的 dialTimeout 同量级）。mihomo 出站适配器自建 dialer 且不设 Timeout——替换掉
// DialContext 后底座的 10s 建连上限彻底失效，黑洞地址要等系统 TCP 重传窗口
// （实测 21s）才失败。main 装配时用 SetNodeDialTimeout(upstream.DialTimeout) 把
// 口径钉到上游单一事实来源（分层硬约束：本包不 import internal/upstream，故只能
// 由 main 注入值，本常量仅作测试/裸用时的安全网）。
const DefaultNodeDialTimeout = 10 * time.Second

// TransportFor 返回绑定某节点的出站 transport；id 空 / 节点不存在 / 已停用 /
// 底座缺失 / 节点解析失败 → nil（调用方回落直连）。结果按节点缓存；节点内容更新
// （同 tag 换服务器）、删除、停用时由调用方失效缓存（CloseIdleConnections）。
func (p *Pool) TransportFor(id string) *http.Transport {
	if id == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok || !n.Enabled {
		return nil
	}
	if tr, ok := p.trCache[id]; ok {
		return tr
	}
	if p.base == nil {
		return nil
	}
	proxy, err := adapter.ParseProxy(n.Raw)
	if err != nil {
		// 落盘的 Raw 都经过入池校验，走到这里说明数据被外部改坏：直连兜底并留痕。
		log.Printf("proxypool: node %s parse failed, fallback to direct: %v", n.Tag, err)
		return nil
	}
	tr := p.base()
	tr.DialContext = dialViaNode(proxy, p.nodeDialTimeout())
	p.trCache[id] = tr
	return tr
}

// nodeDialTimeout 生效的建连上限（SetNodeDialTimeout 未调用 → 默认 10s）。
func (p *Pool) nodeDialTimeout() time.Duration {
	if p.dialTimeout > 0 {
		return p.dialTimeout
	}
	return DefaultNodeDialTimeout
}

// SetNodeDialTimeout 注入节点建连上限（main 装配期传入 upstream.DialTimeout，让
// 「与共享 transport 同参数」成为同一常量而非两处硬编码）。非正值忽略。
func (p *Pool) SetNodeDialTimeout(d time.Duration) {
	if d > 0 {
		p.dialTimeout = d
	}
}

// dialViaNode 把 mihomo 出站适配器包装成 func(ctx, network, addr) 拨号器。
// 每次拨号即一次节点远端连接（与 clash 内核同语义）；network 只支持 TCP 族。
// 建连整体包在 timeout 内（调用方 ctx 更短时以调用方为准）。
func dialViaNode(proxy C.Proxy, timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if timeout <= 0 {
		timeout = DefaultNodeDialTimeout
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("proxypool: unsupported network %q", network)
		}
		host, portS, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("proxypool: bad addr %q: %w", addr, err)
		}
		port, err := strconv.ParseUint(portS, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("proxypool: bad port %q: %w", portS, err)
		}
		md := &C.Metadata{NetWork: C.TCP, DstPort: uint16(port)}
		if ip, ipErr := netip.ParseAddr(host); ipErr == nil {
			md.DstIP = ip.Unmap()
		} else {
			// 域名：交节点远端解析（Host 直传，不落本地 DNS）。
			md.Host = host
		}
		// 建连上限：mihomo 出站不看 net.Dialer.Timeout，只能由 ctx 终止握手。
		// 出站适配器都只在握手期使用 ctx（返回前解除绑定），本层返回后即释放定时器。
		dctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		conn, dialErr := proxy.DialContext(dctx, md)
		if dialErr != nil {
			if ctx.Err() == nil && dctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("proxypool: dial via node timed out after %s: %w", timeout, dialErr)
			}
			return nil, dialErr
		}
		return conn, nil
	}
}
