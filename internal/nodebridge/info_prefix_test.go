package nodebridge

import "testing"

// TestIsInfoOnlyTagResidualReset 机场的套餐文案行不止「剩余流量 / 套餐到期」两种写法，
// 「距离下次重置剩余：27 天」同样只是信息。2026-11-19 线上真把一个号绑到了这种条目上，
// 所以前缀表必须覆盖它；同时不能把真节点名误杀。
func TestIsInfoOnlyTagResidualReset(t *testing.T) {
	info := []string{
		"距离下次重置剩余：27 天",
		"  距离下次重置剩余：3 天",
		"剩余流量：1.89 TB",
		"套餐到期：长期有效",
	}
	for _, s := range info {
		if !IsInfoOnlyTag(s) {
			t.Fatalf("%q 应被判为套餐文案行", s)
		}
	}
	real := []string{
		"🇯🇵日本•移联01",
		"🇸🇬新加坡高速03|BGP|CTCUCM",
		"🇭🇰香港高速02|BGP|CMCU",
	}
	for _, s := range real {
		if IsInfoOnlyTag(s) {
			t.Fatalf("%q 是真节点，不该被过滤", s)
		}
	}
}
