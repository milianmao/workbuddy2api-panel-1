package proxypool

import (
	"path/filepath"
	"testing"
)

// 回归：订阅换一批节点后（机场改名/换 IP → 桥换端口），旧 URL 会永远留在池里，
// 且绑定会指向已消失的节点。Reconcile 必须删掉陈旧节点、并把绑定按节点名迁到
// 同名新节点；迁不了的要解绑回随机池（留着 = fail-closed 把账号卡死）。
func TestReconcilePrunesStaleNodesAndRemapsBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")
	m, err := New(path, nil, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	const (
		oldHK = "socks5://127.0.0.1:39100"
		jp    = "socks5://127.0.0.1:39101"
		newHK = "socks5://127.0.0.1:39102"
	)
	if _, err := m.Import(oldHK + "\n" + jp); err != nil {
		t.Fatalf("import old: %v", err)
	}
	uid := "u-remap"
	if err := m.Bind(uid, Binding{Mode: "fixed", ProxyID: idOfURL(t, m, oldHK)}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := m.Import(newHK); err != nil {
		t.Fatalf("import new: %v", err)
	}
	tags := map[string]string{oldHK: "HK-01", newHK: "HK-01", jp: "JP-01"}
	res, err := m.Reconcile(map[string]bool{newHK: true, jp: true}, func(u string) string { return tags[u] })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("应删除 1 个陈旧节点，得到 %d", res.Removed)
	}
	want := idOfURL(t, m, newHK)
	if got := res.Remapped[uid]; got != want {
		t.Fatalf("绑定应迁到同名新节点 %s，得到 %q（remapped=%v）", want, got, res.Remapped)
	}
	if got := m.Snapshot().Bindings[uid]; got.ProxyID != want || got.Mode != "fixed" {
		t.Fatalf("绑定落盘不对：%+v", got)
	}
	if n := len(m.Snapshot().Proxies); n != 2 {
		t.Fatalf("池应只剩 2 个节点，得到 %d", n)
	}
	// 池文件必须自洽：New 会校验 fixed 绑定的节点存在，悬空绑定直接启动失败。
	if _, err := New(path, nil, nil); err != nil {
		t.Fatalf("对账后重新载入池应成功: %v", err)
	}
}

// 没有同名新节点时：解绑回随机池，而不是留一条指向已消失节点的绑定。
func TestReconcileUnbindsWhenNoReplacement(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "pool.json"), nil, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	const (
		gone = "socks5://127.0.0.1:39110"
		keep = "socks5://127.0.0.1:39111"
	)
	if _, err := m.Import(gone + "\n" + keep); err != nil {
		t.Fatalf("import: %v", err)
	}
	uid := "u-unbind"
	if err := m.Bind(uid, Binding{Mode: "fixed", ProxyID: idOfURL(t, m, gone)}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	res, err := m.Reconcile(map[string]bool{keep: true}, func(u string) string {
		if u == gone {
			return "HK-01" // 新集合里没有 HK-01
		}
		return "JP-01"
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Unbound) != 1 || res.Unbound[0] != uid {
		t.Fatalf("应解绑 1 个账号，得到 %v", res.Unbound)
	}
	if _, ok := m.Snapshot().Bindings[uid]; ok {
		t.Fatalf("解绑后不该留绑定条目")
	}
}

// keep 为空意味着「桥没起来」，此时不敢删（空集合 ≠ 全部陈旧）。
func TestReconcileIgnoresEmptyKeep(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "pool.json"), nil, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if _, err := m.Import("socks5://127.0.0.1:39120"); err != nil {
		t.Fatalf("import: %v", err)
	}
	res, err := m.Reconcile(nil, func(string) string { return "" })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Removed != 0 || len(m.Snapshot().Proxies) != 1 {
		t.Fatalf("keep 为空时不得删除任何节点：removed=%d n=%d", res.Removed, len(m.Snapshot().Proxies))
	}
}

func idOfURL(t *testing.T, m *Manager, u string) string {
	t.Helper()
	for _, v := range m.Snapshot().Proxies {
		if v.URL == u {
			return v.ID
		}
	}
	t.Fatalf("池里没有 %s", u)
	return ""
}

// 整池都被判成陈旧 = 桥换了端口集合（重启/漂移），不是真换了订阅。
// 这时不许删：删完会把所有绑定一起解掉，代价远大于留着几条僵尸行。
func TestReconcileSkipsWhenWholePoolLooksStale(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "pool.json"), nil, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	const a = "socks5://127.0.0.1:39130"
	if _, err := m.Import(a + "\nsocks5://127.0.0.1:39131"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := m.Bind("u1", Binding{Mode: "fixed", ProxyID: idOfURL(t, m, a)}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	res, err := m.Reconcile(map[string]bool{"socks5://127.0.0.1:39999": true}, func(string) string { return "X" })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Skipped == "" {
		t.Fatalf("整池陈旧时应跳过，得到 %+v", res)
	}
	if n := len(m.Snapshot().Proxies); n != 2 {
		t.Fatalf("跳过时不得删节点，剩 %d", n)
	}
	if _, ok := m.Snapshot().Bindings["u1"]; !ok {
		t.Fatalf("跳过时不得解绑")
	}
}
