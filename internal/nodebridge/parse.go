// parse.go 订阅/手贴文本 → clash proxy map 列表（从本 fork 旧 proxypool 抽出来，
// 与上游 #122 的「一行一个代理 URL」模型并存：上游负责路由，本包负责把机场订阅里
// 的 vless/hysteria2 等原生节点变成上游能吃的本地 socks5 出口）。
//
// 三条解析路径（解析一律委托 mihomo 库，不手写 URI 编解码）：
//  1. Clash YAML：正文有 `proxies:` 行（行首锚定）或整段被 base64 包裹的 Clash YAML；
//  2. URI 列表：明文或 base64 的 ss:// vmess:// vless:// trojan:// hysteria2:// …；
//  3. 裸 YAML 列表（- name: ...）兜底。
package nodebridge

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/metacubex/mihomo/common/convert"
	"go.yaml.in/yaml/v3"
)

type yamlDoc struct {
	Proxies []map[string]any `yaml:"proxies"`
}

// infoOnlyTag 机场塞在节点列表里的套餐文案行（「剩余流量：1.89 TB」
// 「套餐到期：长期有效」）：它们带的是同一台机器的真配置，所以 mihomo 解得开、
// 也拨得通，但名字是信息，当成代理只会变成永远不健康、还白占面板一行。
// 只按前缀匹配，不做模糊匹配（免得误杀真节点）。
func infoOnlyTag(name string) bool {
	n := strings.TrimSpace(name)
	for _, p := range []string{"剩余流量", "套餐到期", "到期时间", "过滤掉", "官网地址"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// ParseNodes 把订阅正文/手贴文本解析为 clash proxy map 列表（未校验）。
func ParseNodes(text string) ([]map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty text")
	}
	if hasTopLevelProxiesKey(text) {
		return parseClashYAML(text)
	}
	if decoded, ok := decodeBase64Text(text); ok && hasTopLevelProxiesKey(decoded) {
		return parseClashYAML(decoded)
	}
	maps, err := convert.ConvertsV2Ray([]byte(text))
	if err != nil {
		// 裸 YAML 列表兜底：ConvertsV2Ray 对 `- name: x` 会直接报错。
		var list []map[string]any
		if yamlErr := yaml.Unmarshal([]byte(text), &list); yamlErr == nil && len(list) > 0 {
			return list, nil
		}
		return nil, fmt.Errorf("parse uri list: %w", err)
	}
	if len(maps) == 0 {
		var list []map[string]any
		if yamlErr := yaml.Unmarshal([]byte(text), &list); yamlErr == nil && len(list) > 0 {
			return list, nil
		}
	}
	return maps, nil
}

func parseClashYAML(text string) ([]map[string]any, error) {
	var doc yamlDoc
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}
	return doc.Proxies, nil
}

// hasTopLevelProxiesKey 按行结构判断是否有顶层 `proxies:` 段（不认节点名里的同名子串）。
func hasTopLevelProxiesKey(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		rest, ok := strings.CutPrefix(line, "proxies")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t")
		rest, ok = strings.CutPrefix(rest, ":")
		if !ok {
			continue
		}
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '#' {
			return true
		}
	}
	return false
}

func decodeBase64Text(text string) (string, bool) {
	compact := strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t', ' ':
			return -1
		}
		return r
	}, text)
	if compact == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(compact); err == nil {
			return string(raw), true
		}
	}
	return "", false
}
