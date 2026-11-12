package pool

import (
	"reflect"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 目录探针候选：直连号必须排在绑定号前面，否则字典序第一的那个绑定号一旦钉在坏节点上，
// 每次拉目录都会先撞它一次（2026-11-12 实案：/v1/models 从 22 条掉到 4 条）。
func TestModelProbeUIDsPrefersUnbound(t *testing.T) {
	p := realmPool(t) // cn1、cn2（cn 域）+ g1、g2（global 域）
	p.Add(&auth.Auth{UID: "cn3", Domain: "www.codebuddy.cn"})

	// 字典序 cn1 < cn2 < cn3；cn1 绑了固定代理 → 候选中挪到最后。
	p.SetAccountProxy("cn1", "node-dead")

	got := p.ModelProbeUIDs("cn", ModelProbeLimit)
	want := []string{"cn2", "cn3", "cn1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelProbeUIDs(cn)=%v want %v", got, want)
	}

	// n 限制条数（探针不会无限制换号）。
	if got := p.ModelProbeUIDs("cn", 1); !reflect.DeepEqual(got, []string{"cn2"}) {
		t.Fatalf("限制 1 条=%v want [cn2]", got)
	}

	// 全部绑定：候选照样要给出来（靠调用方换号兜底，不能因为全绑定就没有候选）。
	p.SetAccountProxy("cn2", "node-b")
	p.SetAccountProxy("cn3", "node-c")
	if got := p.ModelProbeUIDs("cn", ModelProbeLimit); len(got) != 3 {
		t.Fatalf("全绑定时候选=%v want 3 个", got)
	}

	// realm 隔离 + 空池。
	if got := p.ModelProbeUIDs("global", ModelProbeLimit); !reflect.DeepEqual(got, []string{"g1", "g2"}) {
		t.Fatalf("global 候选=%v want [g1 g2]", got)
	}
	empty := New("")
	if got := empty.ModelProbeUIDs("cn", ModelProbeLimit); got != nil {
		t.Fatalf("空池候选=%v want nil", got)
	}
}

// 外部谓词说「这个号其实钉着节点」时必须采信：池条目上的 proxyID 只是镜像，
// 绑定真相在 proxypool 的 data/proxy-pool.json 里，两者会不一致（2026-11-12 实案）。
func TestModelProbeUIDsWithPrefersRealBinding(t *testing.T) {
	p := realmPool(t) // cn1、cn2
	p.Add(&auth.Auth{UID: "cn3", Domain: "www.codebuddy.cn"})

	// 镜像全空（池条目不知道有绑定），但真正钉了节点的是 cn1 与 cn3。
	bound := map[string]bool{"cn1": true, "cn3": true}
	got := p.ModelProbeUIDsWith("cn", ModelProbeLimit, func(uid string) bool { return !bound[uid] })
	want := []string{"cn2", "cn1", "cn3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelProbeUIDsWith=%v want %v", got, want)
	}
}
