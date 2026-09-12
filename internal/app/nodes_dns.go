package app

import (
	"fmt"
	"log"
	"net/url"
	"strings"
)

// sing-box DNS 模式。
//
// 背景: 原先硬编码 8.8.8.8 明文 UDP, 而大陆对 8.8.8.8:53 普遍污染/丢包。
// 需要解析的两类域名里:
//   - 目标域名(api.b.ai 等): 代理路径下不在本地解析, 由节点侧解析 —— 不受影响;
//   - 节点服务器域名 + direct 出站的目标域名: 本地解析 —— 正是这一环在拖后腿,
//     解析失败的表现是"节点时通时不通", 极难排查。
//
// 因此默认改成 DoH(阿里), 并保留 local 作为引导与兜底。
const (
	dnsModeSystem = "system"  // 系统解析器
	dnsModeDoHAli = "doh-ali" // 阿里 DoH —— 默认
	dnsModeDoHCF  = "doh-cf"  // Cloudflare DoH
	dnsModeCustom = "custom"  // 自定义 DoH 地址

	defaultDNSMode = dnsModeDoHAli

	dnsBootstrapTag = "dns-bootstrap" // 引导解析器: 只走直连, 绝不依赖任何出站
	dnsDefaultTag   = "dns-default"   // 业务默认解析器
)

// buildNodeDNS 生成 sing-box 的 dns 段与默认解析器 tag。
//
// 关键约束: 节点服务器域名的解析绝不能走节点自己(鸡生蛋), 所以引导解析器
// 只允许 local(系统)或直连 DoH; DoH 自身的服务器域名也交给引导解析器解析。
func buildNodeDNS(cfg *zenConfigData) (map[string]any, string) {
	mode, custom := defaultDNSMode, ""
	if cfg != nil {
		if cfg.DNSMode != "" {
			mode = cfg.DNSMode
		}
		custom = strings.TrimSpace(cfg.DNSCustomDNS)
	}

	if mode == dnsModeSystem {
		return map[string]any{
			"servers": []any{map[string]any{"type": "local", "tag": dnsDefaultTag}},
		}, dnsDefaultTag
	}

	ep := map[string]any{
		"type":            "https",
		"tag":             dnsDefaultTag,
		"domain_resolver": dnsBootstrapTag,
	}
	switch mode {
	case dnsModeDoHCF:
		// 1.1.1.1 的证书自带该 IP 的 SAN, 直接用 IP 不会有 SNI 问题。
		ep["server"] = "1.1.1.1"
	case dnsModeCustom:
		host, port, path, err := parseDoHURL(custom)
		if err != nil {
			log.Printf("  dns: 自定义 DoH 地址无法解析(%v), 回退为阿里 DoH", err)
			ep["server"] = "dns.alidns.com"
			break
		}
		// 用域名而非 IP: 保证 TLS SNI 与证书匹配; 该域名由引导解析器解析。
		ep["server"] = host
		if port != 443 {
			ep["server_port"] = port
		}
		if path != "" && path != "/dns-query" {
			ep["path"] = path
		}
	default: // dnsModeDoHAli
		// 阿里 DoH 的证书只覆盖域名, 写 IP 会因 SNI/证书不匹配失败, 故写域名。
		ep["server"] = "dns.alidns.com"
	}

	return map[string]any{
		"servers": []any{
			map[string]any{"type": "local", "tag": dnsBootstrapTag},
			ep,
		},
	}, dnsDefaultTag
}

// parseDoHURL 解析自定义 DoH 地址, 缺省补全 scheme 与端口。
func parseDoHURL(raw string) (host string, port int, path string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", fmt.Errorf("empty DoH url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, "", err
	}
	if u.Hostname() == "" {
		return "", 0, "", fmt.Errorf("missing host")
	}
	if u.Scheme != "https" {
		return "", 0, "", fmt.Errorf("DoH must use https, got %q", u.Scheme)
	}
	port = 443
	if p := u.Port(); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			return "", 0, "", fmt.Errorf("bad port %q", p)
		}
	}
	path = u.Path
	if path == "" {
		path = "/dns-query"
	}
	return u.Hostname(), port, path, nil
}

// normalizeDNSMode 规整配置里的 DNS 模式。
func normalizeDNSMode(mode string) string {
	switch mode {
	case dnsModeSystem, dnsModeDoHAli, dnsModeDoHCF, dnsModeCustom:
		return mode
	default:
		return defaultDNSMode
	}
}
