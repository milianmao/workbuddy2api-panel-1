package nodebridge

import (
	"os"
	"path/filepath"
	"testing"
)

// subEditDisk 一个订阅 + 一个节点。节点 id 与订阅无关（nodeIDFor 只看
// 类型/服务器/端口/标签），所以换订阅链接时节点 id 必须原地不动——
// 账号池绑定的是节点 id，一动绑定就全掉，那正是「删了重建」做不到的事。
const subEditDisk = `{
  "version": 1,
  "saved": "2026-11-09T10:00:00+08:00",
  "subs": {"old": {"id": "old", "name": "机场A", "url": "https://old.example/sub", "last_sync_at": 1793289665}},
  "nodes": {"n1": {"id": "n1", "tag": "香港01", "type": "socks5", "server": "127.0.0.1", "port": 1080, "sub_id": "old", "enabled": true, "latency_ms": 42, "checked_at": 1793289665, "fails": 0, "raw": {"name": "香港01", "type": "socks5", "server": "127.0.0.1", "port": 1080}}}
}`

func loadSubEditBridge(t *testing.T) (*Bridge, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxies.json")
	if err := os.WriteFile(path, []byte(subEditDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(b.Close)
	return b, path
}

// 只改名：身份不变、不发请求（changed=false）、节点不受影响。
func TestUpdateSubRenameKeepsIdentity(t *testing.T) {
	b, _ := loadSubEditBridge(t)

	sub, changed, err := b.UpdateSub("old", "改名之后", "")
	if err != nil {
		t.Fatalf("UpdateSub: %v", err)
	}
	if changed {
		t.Fatal("只改名称不该被当成链接变化（会触发一次多余的拉取）")
	}
	if sub.ID != "old" {
		t.Fatalf("改名不该换 id，得到 %q", sub.ID)
	}
	if sub.Name != "改名之后" {
		t.Fatalf("名称没改上：%q", sub.Name)
	}
	if sub.URL != "https://old.example/sub" {
		t.Fatalf("空 url 不该清掉原链接，得到 %q", sub.URL)
	}
	if n := b.nodes["n1"]; n == nil || n.SubID != "old" {
		t.Fatalf("节点来源不该被改动：%+v", n)
	}
}

// 换链接：id 不变（订阅身份稳定）、节点来源与节点本身一律不动、新链接落盘。
func TestUpdateSubNewURLKeepsIdentity(t *testing.T) {
	b, path := loadSubEditBridge(t)

	const newURL = "https://new.example/sub?token=abc"
	sub, changed, err := b.UpdateSub("old", "", newURL)
	if err != nil {
		t.Fatalf("UpdateSub: %v", err)
	}
	if !changed {
		t.Fatal("换链接必须报 changed=true，否则调用方不会重新拉取")
	}
	if sub.ID != "old" {
		t.Fatalf("id 必须稳定为 %q，得到 %q（id 是订阅身份，不是链接指纹）", "old", sub.ID)
	}
	if sub.URL != newURL {
		t.Fatalf("链接没改上：%q", sub.URL)
	}
	if sub.Name != "机场A" {
		t.Fatalf("空 name 不该清掉原名称，得到 %q", sub.Name)
	}
	if n := b.nodes["n1"]; n == nil {
		t.Fatal("节点不该消失")
	} else if n.SubID != "old" {
		t.Fatalf("节点来源不该被改动，得到 %q", n.SubID)
	}

	// 落盘：重启后仍是新链接（面板改完就丢是最气人的失败）。
	b.save()
	b2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(b2.Close)
	s, ok := b2.Sub("old")
	if !ok {
		t.Fatal("重新加载后找不到订阅 old")
	}
	if s.URL != newURL {
		t.Fatalf("重新加载后链接 = %q，期望 %q", s.URL, newURL)
	}
	if n := b2.nodes["n1"]; n == nil || n.SubID != "old" {
		t.Fatalf("重新加载后节点来源变了：%+v", n)
	}
}

// 非法输入：不存在的 id、非 http(s)、换成另一个已存在的链接，都必须报错且不改状态。
func TestUpdateSubRejects(t *testing.T) {
	b, _ := loadSubEditBridge(t)

	if _, _, err := b.UpdateSub("nope", "x", ""); err == nil {
		t.Fatal("不存在的订阅应报错")
	}
	if _, _, err := b.UpdateSub("old", "", "ftp://x/y"); err == nil {
		t.Fatal("非 http(s) 链接应报错")
	}
	// 造一个占用目标链接的邻居：换过去必须被拒绝，而不是把两个订阅合成一个。
	other := "https://taken.example/sub"
	b.mu.Lock()
	b.subs[subIDFor(other)] = &Sub{ID: subIDFor(other), Name: "占位", URL: other}
	b.mu.Unlock()
	if _, _, err := b.UpdateSub("old", "", other); err == nil {
		t.Fatal("链接已被其他订阅占用时应报错")
	}
	if _, _, err := b.UpdateSub("old", "", "https://old.example/sub"); err != nil {
		t.Fatalf("换成自己当前的链接不该报错（幂等重试）: %v", err)
	}
	if _, ok := b.subs["old"]; !ok {
		t.Fatal("失败的更新不该删掉原订阅")
	}
	if b.nodes["n1"].SubID != "old" {
		t.Fatal("失败的更新不该改动节点来源")
	}
}
