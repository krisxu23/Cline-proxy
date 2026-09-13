package app

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ============================================================================
// 出站地址校验(SSRF 面)
//
// 订阅链接、通用 Provider 的 Base URL、zen 端点列表这些地址都是**由服务端主动
// 发起请求**的目标, 属于典型的 SSRF 攻击面。此前:
//   - 订阅与 zen 端点只检查了字符串前缀 "http://" / "https://";
//   - Provider 的 baseUrl 连前缀都没查, 只要非空就收下。
// 于是 http://169.254.169.254/latest/meta-data/ 这类云元数据地址可以被直接
// 填进配置, 让网关服务端替攻击者去取 —— 在云主机上部署时会泄露实例凭据。
//
// 这里刻意**不**禁止回环与私网地址: 在本机或局域网跑 Ollama / LM Studio /
// vLLM 再挂到这个网关上是很常见的用法, 一刀切会直接误伤真实需求。所以只封掉
// 不存在合法上游用途的那一类目标: 链路本地网段(含 169.254.169.254 元数据)、
// 未指定地址、以及已知的元数据主机名。
// ============================================================================

// blockedOutboundHosts 已知的云元数据主机名。
var blockedOutboundHosts = map[string]bool{
	"metadata":                 true,
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// validateOutboundURL 校验一个由服务端主动请求的地址。
func validateOutboundURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址无法解析: %q", raw)
	}
	// 大小写不敏感地判定协议, 同时把 file:// / gopher:// / ftp:// 这类
	// 与上游请求无关的协议一并挡掉。
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("协议无效（需 http:// 或 https://）: %q", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("地址缺少主机名: %q", raw)
	}
	if isBlockedOutboundHost(u.Hostname()) {
		return fmt.Errorf("目标地址被拒绝（链路本地 / 云元数据地址不允许作为上游）: %q", raw)
	}
	return nil
}

// isBlockedOutboundHost 主机名是否为不允许的出站目标。
func isBlockedOutboundHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if blockedOutboundHosts[host] {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// 169.254.0.0/16 与 fe80::/10: 链路本地, 云元数据服务就在这一段。
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 0.0.0.0 / :: —— 语义上不是"某个上游", 只会带来歧义。
	if ip.IsUnspecified() {
		return true
	}
	// 100.64.0.0/10: 运营商级 NAT 保留段, 不可能是用户自己的上游。
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

// filterOutboundURLs 逐条校验并归一化(去空白、去尾部斜杠)。
// 返回清理后的列表; 任一非法即整体失败, 不做静默丢弃 —— 静默丢会让用户
// 以为配好了, 实际少了一条。
func filterOutboundURLs(raw []string, what string) ([]string, error) {
	cleaned := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			continue
		}
		if err := validateOutboundURL(u); err != nil {
			return nil, fmt.Errorf("%s %s", what, err.Error())
		}
		cleaned = append(cleaned, u)
	}
	return cleaned, nil
}
