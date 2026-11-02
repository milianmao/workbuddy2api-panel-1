package nodebridge

import "testing"

func TestInfoOnlyTag(t *testing.T) {
	info := []string{"剩余流量：1.89 TB", "套餐到期：长期有效", "到期时间：2027-01-01", " 剩余流量：0 GB"}
	for _, s := range info {
		if !infoOnlyTag(s) {
			t.Fatalf("%q 是机场信息行，应被过滤", s)
		}
	}
	real := []string{"🇯🇵日本高速01|BGP|CUCM", "🇺🇸美国高速05|CTCU|0.1x", "HK-01", "", "🇭🇰香港专线04|BGP|流媒体"}
	for _, s := range real {
		if infoOnlyTag(s) {
			t.Fatalf("%q 是真实节点，不得过滤", s)
		}
	}
}
