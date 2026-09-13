package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// 增强节点测试引擎 — 移植自 freesub 的 sing-box 全协议测活逻辑
//
// freesub 的完整管道是 CI 批处理(48 并发、40 分钟), 这里改成网关的运行时形态:
// 通过节点本地 mixed 入站(已在 sing-box 实例中就绪)做 SOCKS5 拨号, 逐节点
// 跑"活性 → 出口IP → 测速断流 → MITM"四关, 结果存入 nodeHealth 供选路与面板
// 使用。链式前置(chain relay)是 CI 特有的(模拟用户 v2rayN 链式), 运行时不做。
//
// 四关对应 freesub 的 test_single_node:
//   1. 活性探测: 分层超时(generate_204 首击 12s / 重试 4s), 任一成功即活
//   2. 出口 IP: 多路 IP 情报 API(ip.sb / ipinfo.io / ip-api.com), 取第一成功
//   3. 测速断流: Cloudflare 限时下载(5s 预算, 70KB/s 阈值, 3s 空闲=断流)
//   4. MITM 检测: TLS 证书链验证 + Cloudflare trace warp=on 套壳识别
// ============================================================================

// --- 常量(与 freesub main_v2.py 对齐) ---

const (
	// 活性探测
	nodeProbeTimeout      = 12 * time.Second // 首击宽超时, 容纳慢启动节点
	nodeProbeRetryTimeout = 4 * time.Second  // 重试窄超时, 死节点快速放弃

	// 测速
	nodeSpeedBudget      = 5.0   // 测速时间预算(秒)
	nodeSpeedMinBPS      = 70000 // 吞吐 < 70KB/s 判定断流
	nodeSpeedChunkSize   = 65536 // 读块大小
	nodeSpeedIdleTimeout = 3.0   // 空闲 > 3s = 断流签名

	// 出口 IP 查询超时
	nodeIPEchoTimeout = 8 * time.Second

	// 并发: checkAllNodeHealth 按节点规模放大, 基数 nodeTestWorkers, 上限 nodeTestMaxWorkers
	nodeTestWorkers    = 10
	nodeTestMaxWorkers = 48
)

// 活性探测 URL(freesub LIVENESS_URLS, 全部要求代理链路完整)
var nodeLivenessURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://gstatic.com/generate_204",
}

// 出口 IP 情报 URL(freesub IP_ECHO_URLS, 多路冗余)
var nodeIPEchoURLs = []string{
	"https://api.ip.sb/geoip",
	"https://ipinfo.io/json",
	"https://ip-api.com/json/?fields=ip,countryCode,as,asname,isp,hosting,mobile,proxy",
}

// 测速 URL(freesub SPEED_TEST_URLS, 多端点兜底)
var nodeSpeedTestURLs = []string{
	"https://speed.cloudflare.com/__down?bytes=5000000",
	"https://speed.cloudflare.com/__down?bytes=2500000",
}

// Cloudflare trace URL(warp=on 检测套壳节点)
const nodeTraceURL = "https://www.cloudflare.com/cdn-cgi/trace"

// --- 节点测试结果 ---

// nodeTestResult 单节点测试的完整结果, 存入 nodeHealthState
type nodeTestResult struct {
	// 基础
	Alive     bool  `json:"alive"`
	LatencyMs int64 `json:"latencyMs"`

	// 出口信息
	ExitIP      string `json:"exitIp,omitempty"`
	ExitCountry string `json:"exitCountry,omitempty"`
	ExitASN     int64  `json:"exitAsn,omitempty"`
	ExitASNorg  string `json:"exitAsnOrg,omitempty"`
	ExitISP     string `json:"exitIsp,omitempty"`

	// 质量
	SpeedBPS  int64 `json:"speedBps"`
	IsStalled bool  `json:"isStalled"`
	MITMRisk  bool  `json:"mitmRisk"`
	IsWarp    bool  `json:"isWarp"`

	// 分类(测试后填充)
	NetworkType   string `json:"networkType,omitempty"`   // datacenter/residential/mobile/cdn/unknown
	NetConfidence int    `json:"netConfidence,omitempty"` // 0-100

	// IP-API 原始数据(供分类用)
	IPAPIHosting bool `json:"-"`
	IPAPIMobile  bool `json:"-"`
	IPAPIProxy   bool `json:"-"`
}

// --- 核心测试入口 ---

// testNodeComprehensive 经单个节点出口跑完整四关测试。
// 返回 nodeTestResult, 失败(节点不可达)时 Alive=false。
// 所有节点出口都通过 sing-box mixed 入站暴露为本地 SOCKS5, 因此统一用
// socks5ProxyURL(key) 建 SOCKS5 拨号客户端。
func testNodeComprehensive(key string) nodeTestResult {
	result := nodeTestResult{LatencyMs: 99999}

	proxyURL, err := socks5ProxyURL(key)
	if err != nil {
		return result
	}

	proxy := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
	}
	defer proxy.CloseIdleConnections()
	client := &http.Client{
		Transport: proxy,
		Timeout:   nodeProbeTimeout + nodeProbeRetryTimeout + 10*time.Second,
	}

	// === 1) 活性探测: 分层超时 ===
	t0 := time.Now()
	for i, u := range nodeLivenessURLs {
		timeout := nodeProbeTimeout
		if i > 0 {
			timeout = nodeProbeRetryTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("User-Agent", "Go-http-client/2.0")
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
				result.Alive = true
				result.LatencyMs = time.Since(t0).Milliseconds()
				break
			}
		}
	}
	if !result.Alive {
		return result
	}

	// === 2) 出口 IP 检测(多路冗余) ===
	for _, u := range nodeIPEchoURLs {
		ctx, cancel := context.WithTimeout(context.Background(), nodeIPEchoTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip, country, asn, asnOrg, isp := parseIPEcho(u, body)
		if ip == "" {
			continue
		}
		result.ExitIP = ip
		result.ExitCountry = country
		result.ExitASN = asn
		result.ExitASNorg = truncateStr(asnOrg, 120)
		result.ExitISP = truncateStr(isp, 120)
		// ip-api.com 额外提供 hosting/mobile/proxy 布尔标记, 供分类用
		if strings.Contains(u, "ip-api.com") {
			var raw map[string]any
			if json.Unmarshal(body, &raw) == nil {
				result.IPAPIHosting, _ = raw["hosting"].(bool)
				result.IPAPIMobile, _ = raw["mobile"].(bool)
				result.IPAPIProxy, _ = raw["proxy"].(bool)
			}
		}
		break
	}

	// === 3) 测速 + 断流检测 ===
	result.SpeedBPS, result.IsStalled = probeNodeSpeed(client)

	// === 4) MITM + WARP 检测 ===
	result.MITMRisk = probeMITM(client)
	result.IsWarp = probeWarp(client)

	// === 5) 出口网络分类 ===
	result.NetworkType, result.NetConfidence = classifyNodeNetwork(&result)

	return result
}

// --- 出口 IP 解析 ---

var asnRegexp = regexp.MustCompile(`^AS(\d+)\s+(.*)`)

// parseIPEcho 解析 IP 情报 API 响应, 返回 (ip, country, asn, asnOrg, isp)
func parseIPEcho(url string, body []byte) (string, string, int64, string, string) {
	var j map[string]any
	if err := json.Unmarshal(body, &j); err != nil {
		return "", "", 0, "", ""
	}
	// 不同 API 的 IP 字段名不同
	ip := stringFromMap(j, "ip", "query", "your_ip")
	if ip == "" {
		return "", "", 0, "", ""
	}

	var country string
	var asn int64
	var asnOrg, isp string

	switch {
	case strings.HasPrefix(url, "https://api.ip.sb"):
		country = stringFromMap(j, "country_code")
		if v, ok := j["asn"]; ok {
			if f, ok := v.(float64); ok {
				asn = int64(f)
			}
		}
		asnOrg = stringFromMap(j, "asn_organization", "organization")
		isp = stringFromMap(j, "isp", "organization")
	case strings.HasPrefix(url, "https://ipinfo.io"):
		country = stringFromMap(j, "country")
		if country != "" {
			country = strings.ToUpper(country)
		}
		org := stringFromMap(j, "org")
		if m := asnRegexp.FindStringSubmatch(org); m != nil {
			fmt.Sscanf(m[1], "%d", &asn)
			asnOrg = m[2]
		}
		isp = org
	case strings.Contains(url, "ip-api.com"):
		country = stringFromMap(j, "countryCode")
		if country != "" {
			country = strings.ToUpper(country)
		}
		asStr := stringFromMap(j, "as")
		if m := asnRegexp.FindStringSubmatch(asStr); m != nil {
			fmt.Sscanf(m[1], "%d", &asn)
			asnOrg = m[2]
		}
		isp = stringFromMap(j, "isp", "org")
	}

	return ip, country, asn, asnOrg, isp
}

func stringFromMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// --- 测速 + 断流检测 ---

// probeNodeSpeed 经代理做限时下载测速, 返回 (吞吐字节/s, 是否断流)
func probeNodeSpeed(client *http.Client) (int64, bool) {
	for _, url := range nodeSpeedTestURLs {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(nodeSpeedBudget*float64(time.Second)))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var downloaded int64
		t0 := time.Now()
		lastChunk := t0
		chunk := make([]byte, nodeSpeedChunkSize)
		for {
			n, err := resp.Body.Read(chunk)
			if n > 0 {
				downloaded += int64(n)
				lastChunk = time.Now()
			}
			now := time.Now()
			if err != nil {
				break
			}
			if now.Sub(t0).Seconds() > nodeSpeedBudget {
				break
			}
			if now.Sub(lastChunk).Seconds() > nodeSpeedIdleTimeout {
				break // 空闲断流
			}
		}
		resp.Body.Close()
		elapsed := time.Since(t0).Seconds()
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		speed := int64(float64(downloaded) / elapsed)
		if downloaded > 0 {
			stalled := speed < nodeSpeedMinBPS
			return speed, stalled
		}
	}
	// 全部端点都失败 = 断流
	return 0, true
}

// --- MITM + WARP 检测 ---

// probeMITM 经代理访问 gstatic generate_204 并验证 TLS 证书链。
// 证书验证失败 = 中间人劫持; 异常状态码(重定向/403/407/5xx) = 可能被劫持。
func probeMITM(client *http.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeRetryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeLivenessURLs[0], nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		// 超时不算 MITM(节点慢 ≠ 被劫持)
		if ctx.Err() != nil {
			return false
		}
		// TLS 证书验证错误 = MITM 劫持
		errStr := err.Error()
		return strings.Contains(errStr, "certificate") ||
			strings.Contains(errStr, "x509") ||
			strings.Contains(errStr, "tls:")
	}
	resp.Body.Close()
	// 异常状态码 = 可能被劫持(gstatic generate_204 正常返回 204)
	return resp.StatusCode == http.StatusMovedPermanently ||
		resp.StatusCode == http.StatusFound ||
		resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusProxyAuthRequired ||
		resp.StatusCode >= 500
}

// probeWarp 经代理访问 Cloudflare trace, 检测 warp=on 套壳节点。
// Warp 节点会用 Cloudflare WARP 隧道转发流量, trace 响应含 warp=on。
func probeWarp(client *http.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeRetryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeTraceURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "warp=on")
}
