package nodebridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRealLegacyShape 钉住旧版 data/proxies.json 的真实形状。
//
// 踩过的坑：`saved` 在旧版本里是 RFC3339 字符串（"2026-11-01T16:24:14+08:00"），
// 按 int64 声明会让整份文件解析失败，55 个机场节点一个都载不进来、代理池静默变空
// （只有一行 WARN，不看日志根本发现不了）。
func TestLoadRealLegacyShape(t *testing.T) {
	const disk = `{
  "version": 1,
  "saved": "2026-11-01T16:24:14+08:00",
  "subs": {"s1": {"id": "s1", "name": "机场", "url": "https://example.invalid/sub", "last_sync_at": 1793289665}},
  "nodes": {"n1": {"id": "n1", "tag": "香港01", "type": "socks5", "server": "127.0.0.1", "port": 1080, "sub_id": "s1", "enabled": true, "latency_ms": 42, "checked_at": 1793289665, "fails": 0, "raw": {"name": "香港01", "type": "socks5", "server": "127.0.0.1", "port": 1080}}}
}`
	path := filepath.Join(t.TempDir(), "proxies.json")
	if err := os.WriteFile(path, []byte(disk), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(b.Close)
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	lines := b.Lines()
	if !strings.HasPrefix(lines, "socks5://127.0.0.1:") {
		t.Fatalf("Lines=%q，期望一条本地 socks5 出口", lines)
	}
	m, ok := b.Meta(lines)
	if !ok {
		t.Fatalf("Meta(%q) 查不到节点", lines)
	}
	if m.Tag != "香港01" || m.Type != "socks5" || m.Server != "127.0.0.1" || m.Port != 1080 || m.SubID != "s1" {
		t.Fatalf("Meta=%+v", m)
	}
	// 关键回归：远端端口不能被本地监听端口覆盖，否则重启后身份哈希漂移、绑定全断。
	if u, ok := b.URLOf("n1"); !ok || u != lines {
		t.Fatalf("URLOf(n1)=%q ok=%v，期望 %q", u, ok, lines)
	}
	if subs := b.Subs(); len(subs) != 1 || subs[0].ID != "s1" || subs[0].Name != "机场" {
		t.Fatalf("Subs=%+v", subs)
	}

	// 同一份文件重复 Load+Start 必须给出同一个本地端口（绑定与健康记录依赖这点）。
	b2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.Close)
	b.Close()
	if err := b2.Start(); err != nil {
		t.Fatal(err)
	}
	if got := b2.Lines(); got != lines {
		t.Fatalf("重启后出口 URL 漂移：%q != %q", got, lines)
	}
}
