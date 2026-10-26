// parse.go 订阅/手贴文本 → clash proxy map 列表。
//
// 三条解析路径（架构定案：解析委托 mihomo 库，不手写 URI 编解码）：
//  1. Clash YAML：正文有 `proxies:` **行**（行首锚定，不认节点名里的同名子串）或
//     整段 body 被 base64 包裹的 Clash YAML（常见机场形态）→ yaml.Unmarshal 取
//     proxies 数组；
//  2. URI 列表：明文或 base64 的 ss:// vmess:// vless:// trojan:// hysteria2:// 等
//     → convert.ConvertsV2Ray（mihomo 官方转换器，兼容明文/Std/Raw base64 输入）；
//  3. 裸 YAML 列表（- name: ...）兜底。
//
// URI 路径产物的 port 是 string，YAML 路径是 int——buildNode/mapInt 统一归一化。
package proxypool

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	"go.yaml.in/yaml/v3"
)

// yamlDoc Clash 订阅的取用段（只关心 proxies，其余字段忽略）。
type yamlDoc struct {
	Proxies []map[string]any `yaml:"proxies"`
}

// ParseNodes 把订阅正文/手贴文本解析为 clash proxy map 列表（未校验；校验由
// validateNodes 做）。判定顺序：行首 `proxies:` 的 Clash YAML → base64 包裹的
// Clash YAML → URI 列表（明文/base64）→ 裸 YAML 列表兜底。
func ParseNodes(text string) ([]map[string]any, error) {
	text = trimSpace(text)
	if text == "" {
		return nil, errors.New("empty text")
	}
	if hasTopLevelProxiesKey(text) {
		return parseClashYAML(text)
	}
	// base64 包裹整套 Clash YAML（机场常见形态：整段订阅 body 编码后下发）。
	// 先试解码再看是否含 proxies: 段，避免把 base64 URI 列表误当 YAML。
	if decoded, ok := decodeBase64Text(text); ok && hasTopLevelProxiesKey(decoded) {
		return parseClashYAML(decoded)
	}
	// ConvertsV2Ray 接受 []byte：base64 正文与明文 URI 行都行（内部先试 base64
	// 解码，失败按明文逐行处理；无效行静默跳过）。
	maps, err := convert.ConvertsV2Ray([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("parse uri list: %w", err)
	}
	if len(maps) == 0 {
		// 非 URI、非 proxies: 段：可能是一段裸 YAML 列表（- name: ...），兜底再试。
		var list []map[string]any
		if yamlErr := yaml.Unmarshal([]byte(text), &list); yamlErr == nil && len(list) > 0 {
			return list, nil
		}
	}
	return maps, nil
}

// parseClashYAML 解析 Clash 订阅取 proxies 段。
func parseClashYAML(text string) ([]map[string]any, error) {
	var doc yamlDoc
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}
	return doc.Proxies, nil
}

// hasTopLevelProxiesKey 判断正文是否有 `proxies:` 段（行首锚定、允许缩进 0 的顶层键）。
//
// 为什么不能 strings.Contains：明文 URI 的节点名/备注里出现 "proxies:" 子串极常见
// （如 `ss://...#my-proxies:list`），误判会把 URI 列表送进 yaml.Unmarshal → 报错
// 或解析出空段；「有 proxies 键」必须按行结构判定。容忍行尾注释与前导空白。
func hasTopLevelProxiesKey(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		// 缩进行 = 嵌套键（如 proxy-groups 项里的 proxies），不是订阅的顶层段。
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		rest, ok := strings.CutPrefix(line, "proxies")
		if !ok {
			continue
		}
		// YAML 允许键与冒号之间有空白（`proxies :`）。
		rest = strings.TrimLeft(rest, " \t")
		rest, ok = strings.CutPrefix(rest, ":")
		if !ok {
			continue
		}
		// 键后必须是空白/注释/行尾，避免 "proxies:x" 这类前缀相同的怪键。
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '#' {
			return true
		}
	}
	return false
}

// decodeBase64Text 尝试把整段正文按 base64（Std/Raw/URL 变体，容忍换行）解码。
// 失败或解出来不是可读文本时返回 ok=false（调用方继续走 URI 路径）。
func decodeBase64Text(text string) (string, bool) {
	// 机场常在 base64 里塞换行/空白：先剥掉。
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

// validateNodes 逐 map 校验可被 mihomo 解析（adapter.ParseProxy 即真实构造一次
// 出站适配器，等于把「格式垃圾」挡在入池前），并装配成 Node。返回可用节点与
// 逐条失败原因（下标与解析顺序对应，带 tag 便于定位）。
func validateNodes(maps []map[string]any, subID string, taken map[string]bool) (nodes []*Node, errs []string) {
	for _, m := range maps {
		name, _ := m["name"].(string)
		if _, err := adapter.ParseProxy(m); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", displayTag(name, m), err))
			continue
		}
		tag := allocTag(trimSpace(name), taken)
		// ParseProxy 成功但字段缺失的残缺 map（理论上不会出现）由 buildNode 兜底拦截。
		n, err := buildNode(m, tag, subID)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, errs
}

// displayTag 错误信息里的节点称呼：name 缺失时用 type@server 兜底。
func displayTag(name string, m map[string]any) string {
	if t := trimSpace(name); t != "" {
		return t
	}
	typ, _ := m["type"].(string)
	server, _ := m["server"].(string)
	if typ == "" && server == "" {
		return "(unnamed)"
	}
	return fmt.Sprintf("(unnamed %s@%s)", typ, server)
}
