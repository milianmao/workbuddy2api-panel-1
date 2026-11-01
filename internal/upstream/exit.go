// exit.go 出站报文形态的最后一公里（我们的统一出口）。
//
// 上游把「头名拼写对齐（headcase.go）」和「补回官方必发的 Connection 头」挂在
// internal/upstream/proxy.go 的 do() 里——那是上游代理池带来的单一出站收口。
// 本 fork 没有那个 do()：账号级代理池走 clientFor(a) 取 client、调用点直接
// client.Do(req)，出站点是 13 个而不是 1 个。所以这里用 sendAligned 做同一个
// 收口，语义与上游 do() 完全一致（先纠拼写，再补 Connection，最后才发）。
package upstream

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// officialConnectionValue 给出该 path 上官方客户端实际发送的 Connection 值。
//
// 2026-10-30 双版本真机抓包（CN 5.9.3 / GL 5.9.4）：官方在每条 RPC 上都显式
// 写了 Connection —— 除 /v3/config 是 close 外，其余全是 keep-alive。
// HTTP/1.1 默认即复用，Go 因此从不发送这一行，但头集合少了它就是少一个字段；
// 风控按白名单查头时，"缺一个官方必发的头"和"多一个官方不发的头"一样显眼。
//
// 没抓到过的 path 返回空串，保持 Go 默认行为，不臆造。
func officialConnectionValue(path string) string {
	switch path {
	case "/v3/config":
		return "close"
	case "/v2/chat/completions", "/v2/chat/busy", "/v2/report":
		return "keep-alive"
	}
	return ""
}

// applyConnectionHeader 把官方那个显式的 Connection 头补回去。
//
// close 不能写进 Header：net/http 的 transferWriter 会按 req.Close 自己写一行
// Connection: close，手写会重复成两行。keep-alive 相反 —— Go 认为它是默认值所以
// 从不发出，必须手写才会出现在线上字节里。
func applyConnectionHeader(req *http.Request) {
	switch officialConnectionValue(req.URL.Path) {
	case "close":
		req.Close = true
	case "keep-alive":
		req.Header.Set("Connection", "keep-alive")
	}
}

// applyOfficialHead 把一个请求的头改成官方线上形态（拼写 + Connection）。
// normalizeHeaderSpelling 会 Clone 一份，故调用方原 req 不被修改。
func applyOfficialHead(req *http.Request) *http.Request {
	req = normalizeHeaderSpelling(req)
	applyConnectionHeader(req)
	return req
}

// sendAligned 是本包唯一的「发出去」入口：先补头形态再发。
// 新增出站点请一律走这里，别直接 client.Do(req) —— 直接发就漏掉头形态对齐。
func sendAligned(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(applyOfficialHead(req))
}

// do 是上游的「单一出站口」（他们代理池带来的收口）。本 fork 的账号级出口是
// clientFor(a)，但上游自带的测试与探针都按 do(req, a, chat) 调用——保留同签名，
// 让那些用例原样跑，实现仍是我们那套：按账号取 client，再走 sendAligned。
// 出口顺序固定：先按路径补头形态（拼写 + Connection），再交 #122 代理池按 uid
// 选节点。池子未装配（ProxyPool == nil）或 a 为空时退回原直连底座——与上游
// do() 的分支一致，保证未启用代理池时行为零变化。
func (c *Client) do(req *http.Request, a *auth.Auth, chat bool) (*http.Response, error) {
	regular, chatClient := c.clientFor(a)
	client := regular
	if chat {
		client = chatClient
	}
	prepared := applyOfficialHead(req)
	if c.ProxyPool == nil || a == nil {
		return client.Do(prepared)
	}
	return c.ProxyPool.Do(client, prepared, a.UID)
}
