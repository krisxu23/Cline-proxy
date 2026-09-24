package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
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
// 这里对**链路本地/未指定/元数据主机名**永远拦截(不存在合法上游用途);
// 对**私网与回环**默认也拦截(审计 P3-10: 否则网关等于一台内网探测器),
// 但在本机或局域网跑 Ollama / LM Studio / vLLM 的用户可以用
// FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM=1 显式放开 —— 一刀切会误伤真实需求,
// 但"默认放开"又不该是安全默认值。
// ============================================================================

// AllowPrivateUpstreamEnv 放开私网/回环上游的环境变量(取值 1/true/yes)。
const AllowPrivateUpstreamEnv = "FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM"

// privateUpstreamAllowed 是否允许把私网/回环地址当作上游。
func privateUpstreamAllowed() bool {
	v := strings.TrimSpace(os.Getenv(AllowPrivateUpstreamEnv))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

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
	if reason := blockedOutboundReason(u.Hostname()); reason != "" {
		return fmt.Errorf("目标地址被拒绝（%s）: %q", reason, raw)
	}
	if reason := resolvedLinkLocalReason(u.Hostname()); reason != "" {
		return fmt.Errorf("目标地址被拒绝（%s）: %q", reason, raw)
	}
	return nil
}

// resolvedLinkLocalReason 主机名解析结果里是否有链路本地地址(169.254.0.0/16 /
// fe80::/10)。返回拒绝原因; 允许或无法判定时返回空串。
//
// 主机名**字面量**检查拦不住"名字解析到云 metadata"的 DNS rebinding 面(P3-28):
// 这里在拿到解析结果之后、建立任何连接之前把解析结果拦下。**只拦链路本地段**
// (云 metadata 服务就在这一段); 私网(10/8、172.16/12、192.168/16、fc00::/7)
// 与回环**绝不在此拦** —— 局域网/LAN 上游是正当用途, 它们的拦截策略由
// blockedOutboundReason 按 FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM 单独管理。
// 解析失败(离线/NXDOMAIN)在配置期判定不了, 不拦。
func resolvedLinkLocalReason(host string) string {
	if net.ParseIP(host) != nil {
		return "" // IP 字面量已由 blockedOutboundReason 判过, 无需再解析
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return "主机名解析到链路本地/云元数据地址 " + ip.String()
		}
		// 私网/回环的解析后复检(2026-09-24 审查): 字面量路径
		// (blockedOutboundReason)本来就拦这两类, 但**域名**解析到它们此前是漏的
		// —— "配一个公网域名、实际解析到 10/172.168/127" 即可把网关当内网探针。
		// 这里补齐, 口径与字面量路径完全一致(含 FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM
		// 放开开关)。
		//
		// ★ 只在**配置期**做, 拨号期绝不做: dialViaProxy 对每个节点拨的都是
		// 127.0.0.1:<节点端口>(本地 sing-box 入站), 在拨号层拦回环会直接打断
		// 整个节点池。见 dialWithSSRFGuard 的注释。
		if !privateUpstreamAllowed() {
			if ip.IsLoopback() {
				return "主机名解析到回环地址 " + ip.String() + "（本机自建上游请设 " + AllowPrivateUpstreamEnv + "=1）"
			}
			if ip.IsPrivate() {
				return "主机名解析到私网地址 " + ip.String() + "（内网自建上游请设 " + AllowPrivateUpstreamEnv + "=1）"
			}
		}
	}
	return ""
}

// blockedOutboundReason 返回该主机被拒的原因; 允许时返回空串。
func blockedOutboundReason(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if blockedOutboundHosts[host] {
		return "云元数据主机名不允许作为上游"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	// 169.254.0.0/16 与 fe80::/10: 链路本地, 云元数据服务就在这一段。
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "链路本地地址 / 云元数据地址不允许作为上游"
	}
	// 0.0.0.0 / :: —— 语义上不是"某个上游", 只会带来歧义。
	if ip.IsUnspecified() {
		return "未指定地址(0.0.0.0/::)不是有效的上游"
	}
	// 100.64.0.0/10: 运营商级 NAT 保留段, 不可能是用户自己的上游。
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return "运营商级 NAT 保留段不是有效的上游"
	}
	if !privateUpstreamAllowed() {
		if ip.IsLoopback() {
			return "回环地址默认不允许作为上游（本机自建上游请设 " + AllowPrivateUpstreamEnv + "=1）"
		}
		if ip.IsPrivate() {
			return "私网地址默认不允许作为上游（内网自建上游请设 " + AllowPrivateUpstreamEnv + "=1）"
		}
	}
	return ""
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

// dialWithSSRFGuard 给拨号函数包一层运行时 SSRF 防线: 配置期校验只覆盖写入
// 那一刻, 之后域名重新解析到 169.254.169.254(DNS rebinding)会绕开它 —— 拨号
// 前后按 addr 字面量与**实际连接对端**各查一次, 堵住配置期到拨号期的窗口。
//
// 刻意只拦两类: 链路本地(169.254.0.0/16 / fe80::/10, 云 metadata 服务就在
// 这一段)与 blockedOutboundHosts 里的云元数据主机名字面量。回环/私网**绝不在
// 此拦** —— 本地 sing-box 与 LAN 上游是正当用途, 它们的策略由
// blockedOutboundReason 按 FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM 管理。
// 拦下时关闭已建连接并返回错误, 口径与 blockedOutboundReason /
// resolvedLinkLocalReason 一致。
func dialWithSSRFGuard(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 拨号前: addr 里的主机名字面量/字面 IP 先判, 不给链路本地任何建连机会。
		if host, _, err := net.SplitHostPort(addr); err == nil {
			host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if blockedOutboundHosts[host] {
				return nil, fmt.Errorf("目标地址被拒绝（云元数据主机名不允许作为上游）: %q", addr)
			}
			if ip := net.ParseIP(host); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				return nil, fmt.Errorf("目标地址被拒绝（链路本地地址 / 云元数据地址不允许作为上游）: %q", addr)
			}
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		// 拨号后: 域名可能刚被重绑定到云 metadata 段, 按实际对端 IP 再查一次
		// (配置期 resolvedLinkLocalReason 只保证那一刻的解析结果干净)。
		if ra, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			if i := strings.IndexByte(ra, '%'); i >= 0 {
				ra = ra[:i] // IPv6 zone(fe80::1%eth0)不参与判定
			}
			if ip := net.ParseIP(ra); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				conn.Close()
				return nil, fmt.Errorf("目标地址被拒绝（主机名解析到链路本地/云元数据地址 %s）", ip)
			}
		}
		return conn, nil
	}
}
