package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
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
//   1. 活性探测: 三权威源并发(gstatic 明文 204 / CF 204 / Apple  captive 200),
//      任一成功即活, 延迟取最快成功者
//   2. 出口 IP: 三路 IP 情报 API 并发, 国家码少数服从多数投票, 附带信息取多数票源
//   3. 测速断流: Cloudflare 限时下载(5s 预算, 70KB/s 阈值, 3s 空闲=断流)
//   4. MITM 检测: TLS 证书链验证 + Cloudflare trace warp=on 套壳识别
// ============================================================================

// --- 常量(与 freesub main_v2.py 对齐) ---

const (
	// 活性探测
	nodeProbeTimeout      = 12 * time.Second // 单源宽超时, 容纳慢启动节点
	nodeProbeRetryTimeout = 4 * time.Second  // MITM/WARP 复核窄超时, 死节点快速放弃

	// 测速(2026-09-22 审查: 原 nodeSpeedMinBPS=70000 直接判死, 实测误杀
	// 12~62KB/s 的可用节点。按用户决定降到 50KB/s 并改判**降权**, 不判死)
	nodeSpeedBudget      = 5.0   // 测速时间预算(秒)
	nodeSpeedSlowBPS     = 50000 // < 50KB/s 只在选路里降权兜底, 不判死
	nodeSpeedChunkSize   = 65536 // 读块大小
	nodeSpeedIdleTimeout = 3.0   // 读空闲 > 3s = 真断流签名(唯一判死条件)

	// 出口 IP 查询超时
	nodeIPEchoTimeout = 8 * time.Second

	// 并发: checkAllNodeHealth 按节点规模放大, 基数 nodeTestWorkers, 上限 nodeTestMaxWorkers
	nodeTestWorkers    = 32
	nodeTestMaxWorkers = 128
)

// 活性探测 URL(三权威 captive-portal 探测源, 并发取最快成功):
//   - gstatic 明文 generate_204: 经典探测, 回 204 即链路通(明文是故意的,
//     探测体本身不含任何敏感信息, 且 captive 门户只劫持明文)
//   - gstatic generate_204: 老牌 captive 源
//   - Cloudflare generate_204: 第二独立源, 回 204 即活
//   - Apple captive: 回 200(体为 Success 文本, 只认状态码不校验体)即活
//
// ⚠️⚠️ 2026-09-18 实测修正: **绝不能再用 gstatic / apple 当探测目标**。
//
//	本机(广东)所有国内解析器对 `www.gstatic.com` 一律返回**中国联通 IP**
//	(`58.254.137.162` / `58.254.149.162`; doh.pub、dns.alidns.com、119.29.29.29、
//	223.5.5.5、114.114.114.114 全都一样), `captive.apple.com` 返回国内 IPv6。
//	也就是说: 无论把 dnsMode 换成哪个国内 DoH 都躲不开。
//
//	后果有两层, 合起来就是"一个有效节点都没有":
//	  1. 出口拿着这个**国内 IP** 去连, 连不上 → 活性探测失败;
//	  2. 更致命的是 MITM 探测(见 nodeMITMURL)打的也是这个域名 —— 出口连到
//	     国内 IP, TLS 证书对不上 → 判定"被劫持" → MITM_Risk=true →
//	     健康判定 `Alive && !MITMRisk && !IsStalled` **对每个节点都返回 false**。
//
//	修法: 换成 (a) **IP 字面量**(DNS 无从污染) 与 (b) 国内也能正确解析到真实
//	Cloudflare(AS13335) 的端点。`cp.cloudflare.com` 实测解析为
//	`2606:4700::6810:84e5`(真实 Cloudflare)且本机直连 204。
//
// 探测 URL 均可被环境变量覆盖(默认行为不变): 内网/代理场景下公网权威源不可达时,
// 无需改代码即可指向自建探针。逗号分隔, 空项忽略, 全空则保留默认值。
//
//	FREE_ROUTER_LIVENESS_URLS / FREE_ROUTER_MITM_URL / FREE_ROUTER_IP_ECHO_URLS /
//	FREE_ROUTER_SPEEDTEST_URLS / FREE_ROUTER_TRACE_URL
var nodeLivenessURLs = []string{
	"https://1.1.1.1/cdn-cgi/trace",          // IP 字面量: 完全不经过 DNS, 污染无从下手
	"https://cp.cloudflare.com/generate_204", // 国内解析正确(真实 Cloudflare)
	"https://www.cloudflare.com/cdn-cgi/trace",
}

// nodeMITMURL MITM 复核专用: 必须走 HTTPS 才能验证 TLS 证书链。
//
// 用 **IP 字面量**: 1.1.1.1 的证书自带该 IP 的 SAN, 因此既不需要 DNS(污染无
// 从下手), 又能真正验证证书链 —— 被中间设备劫持时证书必然对不上, 检出能力不减。
// (旧值 https://www.gstatic.com/generate_204 会被解析到国内 IP, 导致合法节点
//
//	被误判为 MITM —— 见上方 nodeLivenessURLs 的说明。)
var nodeMITMURL = "https://1.1.1.1/cdn-cgi/trace"

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
var nodeTraceURL = "https://www.cloudflare.com/cdn-cgi/trace"

// envCSVList 读逗号分隔的环境变量, 有值才返回(调用方决定是否覆盖默认值)。
func envCSVList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func init() {
	if v := envCSVList("FREE_ROUTER_LIVENESS_URLS"); len(v) > 0 {
		nodeLivenessURLs = v
	}
	if v := strings.TrimSpace(os.Getenv("FREE_ROUTER_MITM_URL")); v != "" {
		nodeMITMURL = v
	}
	if v := envCSVList("FREE_ROUTER_IP_ECHO_URLS"); len(v) > 0 {
		nodeIPEchoURLs = v
	}
	if v := envCSVList("FREE_ROUTER_SPEEDTEST_URLS"); len(v) > 0 {
		nodeSpeedTestURLs = v
	}
	if v := strings.TrimSpace(os.Getenv("FREE_ROUTER_TRACE_URL")); v != "" {
		nodeTraceURL = v
	}
}

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
	// SpeedTestFailed 测速端点全挂/0 字节 —— **不是**节点的错, 不参与健康判定,
	// 也不算慢(2026-09-22 审查 P1: 曾被无差别并入 IsStalled 判死)。
	SpeedTestFailed bool `json:"speedTestFailed,omitempty"`
	MITMRisk        bool `json:"mitmRisk"`
	IsWarp          bool `json:"isWarp"`

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
//
// 统一规则(P0 修复): 探测请求的 cancel 必须发生在响应体读尽并 Close 之后 ——
// Go 里请求 ctx 一旦取消, 后续 Body.Read 立刻返回 context canceled, 哪怕
// client.Do 已经成功返回。旧写法在 Do 之后马上 cancel(), 导致出口 IP 响应
// 读不到、测速恒为 0 字节并一律判"断流", 节点明明活着却整体检测失败。
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

	// === 1) 活性探测: 三权威源并发, 任一成功即活 ===
	if lat, ok := probeNodeLiveness(client); ok {
		result.Alive = true
		result.LatencyMs = lat
	} else {
		return result
	}

	// === 2) 出口 IP 检测(三源并发 + 国家码多数投票) ===
	probeNodeExitInfo(client, &result)

	// === 3) 测速 + 断流检测 ===
	result.SpeedBPS, result.IsStalled, result.SpeedTestFailed = probeNodeSpeed(client)

	// === 4) MITM + WARP 检测 ===
	result.MITMRisk = probeMITM(client)
	result.IsWarp = probeWarp(client)

	// === 5) 出口网络分类 ===
	result.NetworkType, result.NetConfidence = classifyNodeNetwork(&result)

	return result
}

// --- 活性探测(三权威源并发) ---

// probeNodeLiveness 并发探测三权威 captive 源, 返回 (最快成功延迟ms, 是否存活)。
// 任一源回 204/200 即活; 三源全失败才判死。各源独立超时互不阻塞, 整体耗时
// 取决于最快成功者而非最慢源。cancel 发生在各自响应体读尽并 Close 之后,
// 与 P0 修复(见 testNodeComprehensive 注释)同一规则。
func probeNodeLiveness(client *http.Client) (int64, bool) {
	t0 := time.Now()
	type liveResult struct {
		lat int64
		ok  bool
	}
	// 每个探测 goroutine 必发一条回执(成功或失败), 主循环收齐即判 ——
	// 旧写法只在成功时发送, 全死时主循环收不到任何回执, 只能等满兜底
	// 超时才返回(2026-09-16 回归)。
	ch := make(chan liveResult, len(nodeLivenessURLs))
	for _, u := range nodeLivenessURLs {
		go func(url string) {
			send := func(lat int64, ok bool) {
				ch <- liveResult{lat: lat, ok: ok}
			}
			ctx, cancel := context.WithTimeout(context.Background(), nodeProbeTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				send(0, false)
				return
			}
			req.Header.Set("User-Agent", "Go-http-client/2.0")
			resp, err := client.Do(req)
			if err != nil {
				send(0, false)
				return
			}
			// 只认状态码不读体: 204 空体 / Apple 200 Success 文本都只看码,
			// 避免体格式漂移误杀。
			ok := resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if !ok {
				send(0, false)
				return
			}
			send(time.Since(t0).Milliseconds(), true)
		}(u)
	}
	// 首个成功即返回, 不等慢源; 全失败时收齐三条回执即判死, 不等超时。
	// 慢源 goroutine 受各自 ctx 超时约束, 最多存活 nodeProbeTimeout 后自行
	// 退出, 发送时 channel 带缓冲不会阻塞, 不泄漏。
	for range nodeLivenessURLs {
		if r := <-ch; r.ok {
			return r.lat, true
		}
	}
	return 0, false
}

// --- 出口 IP 解析 ---

// exitVote 单源的解析结果(供投票用)。
type exitVote struct {
	ip, country     string
	asn             int64
	asnOrg, isp     string
	hosting, mobile bool
	proxy           bool
	hasNetFlags     bool // 是否携带 hosting/mobile/proxy 标记(ip-api.com 专有)
}

// probeNodeExitInfo 并发查询三路 IP 情报, 国家码少数服从多数后写入 result:
// 三票一致取该地区; 两票一致取多数票; 三票各异或有效票为零归空(调用方按
// other 处理)。ASN/ISP/出口IP 取多数票源, 持平时取先返回者。
func probeNodeExitInfo(client *http.Client, result *nodeTestResult) {
	type indexed struct {
		v exitVote
	}
	ch := make(chan indexed, len(nodeIPEchoURLs))
	var wg sync.WaitGroup
	for _, u := range nodeIPEchoURLs {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), nodeIPEchoTimeout)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				cancel()
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				return
			}
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			cancel()
			if readErr != nil || resp.StatusCode != http.StatusOK {
				return
			}
			ip, country, asn, asnOrg, isp := parseIPEcho(url, body)
			if ip == "" {
				return
			}
			v := exitVote{
				ip:      ip,
				country: strings.ToUpper(strings.TrimSpace(country)),
				asn:     asn,
				asnOrg:  truncateStr(asnOrg, 120),
				isp:     truncateStr(isp, 120),
			}
			// ip-api.com 额外提供 hosting/mobile/proxy 布尔标记, 供分类用
			if strings.Contains(url, "ip-api.com") {
				var raw map[string]any
				if json.Unmarshal(body, &raw) == nil {
					v.hosting, _ = raw["hosting"].(bool)
					v.mobile, _ = raw["mobile"].(bool)
					v.proxy, _ = raw["proxy"].(bool)
					v.hasNetFlags = true
				}
			}
			ch <- indexed{v: v}
		}(u)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	var votes []exitVote
	for it := range ch {
		votes = append(votes, it.v)
	}
	if len(votes) == 0 {
		return
	}
	// 国家码计票(空国家码不计票, 但该票的 IP/ASN 仍可作为附带信息候选)。
	counts := map[string]int{}
	for _, v := range votes {
		if v.country != "" {
			counts[v.country]++
		}
	}
	winner, best := "", 0
	tie := false
	for cc, n := range counts {
		if n > best {
			best, winner, tie = n, cc, false
		} else if n == best {
			tie = true
		}
	}
	// 三票各异(best==1 且票数>1)或零有效票 → 无胜者, 地区留空归 other。
	if winner == "" || (tie && best == 1 && len(counts) > 1) {
		// 地区无法判定, 但出口 IP 仍有价值(去重折叠/面板展示用): 取首票。
		result.ExitIP = votes[0].ip
		result.ExitASN = votes[0].asn
		result.ExitASNorg = votes[0].asnOrg
		result.ExitISP = votes[0].isp
		return
	}
	result.ExitCountry = winner
	// 附带信息取"投给胜者且最先返回"的那票, 找不到则取首票。
	chosen := votes[0]
	for _, v := range votes {
		if v.country == winner {
			chosen = v
			break
		}
	}
	result.ExitIP = chosen.ip
	result.ExitASN = chosen.asn
	result.ExitASNorg = chosen.asnOrg
	result.ExitISP = chosen.isp
	if chosen.hasNetFlags {
		result.IPAPIHosting = chosen.hosting
		result.IPAPIMobile = chosen.mobile
		result.IPAPIProxy = chosen.proxy
	}
}

var asnRegexp = regexp.MustCompile(`^AS(\d+)\s+(.*)`)

// parseIPEcho 解析 IP 情报 API 响应, 返回 (ip, country, asn, asnOrg, isp)。
// 按响应体的字段形状识别格式, 不依赖请求 URL —— 三源并发投票与本地测试桩
// 的 URL 都不是官方域名, 按 URL 前缀分支会把有效响应判废(2026-09-16 回归)。
func parseIPEcho(url string, body []byte) (string, string, int64, string, string) {
	_ = url // 保留签名兼容旧单源调用; 格式识别只看字段
	var j map[string]any
	if err := json.Unmarshal(body, &j); err != nil {
		return "", "", 0, "", ""
	}
	// 不同 API 的 IP 字段名不同
	ip := stringFromMap(j, "ip", "query", "your_ip")
	if ip == "" {
		return "", "", 0, "", ""
	}

	// 国家码: ip.sb(country_code) / ip-api(countryCode) / ipinfo(country, 小写)
	country := stringFromMap(j, "country_code", "countryCode", "country")
	country = strings.ToUpper(strings.TrimSpace(country))

	var asn int64
	var asnOrg, isp string

	// ASN: ip.sb 直接给数字 asn; ipinfo/ip-api 藏在 "AS1234 ORG" 字符串里
	if v, ok := j["asn"]; ok {
		if f, ok := v.(float64); ok {
			asn = int64(f)
		}
	}
	if asn == 0 {
		for _, k := range []string{"as", "org"} {
			if s := stringFromMap(j, k); s != "" {
				if m := asnRegexp.FindStringSubmatch(s); m != nil {
					fmt.Sscanf(m[1], "%d", &asn)
					if asnOrg == "" {
						asnOrg = m[2]
					}
					break
				}
			}
		}
	}
	if asnOrg == "" {
		asnOrg = stringFromMap(j, "asn_organization", "organization", "org")
	}
	isp = stringFromMap(j, "isp", "organization", "org")

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

// probeNodeSpeed 经代理做限时下载测速, 返回 (吞吐字节/s, 是否断流, 测速是否失败)。
//
// 三态语义(2026-09-23 审查 P1 修订):
//   - 断流(IsStalled): **单次 Read 耗时 >3s**(不论是否读到数据) —— 读前记时刻
//     直接量这一次 Read 卡了多久; 旧写法拿与上次 chunk 的时间差判, 而 Read 返回
//     数据后差值刚被刷新、恒 ≈0, 判定实际不可达。预算 ctx 超时(5s>3s)时被卡住
//     的那次 Read 同样落在此判定内, 无进展的断流不会漏 —— 这是节点自身的"断流
//     签名", 唯一足以判死的信号;
//   - 测速失败(SpeedTestFailed): 所有端点都连不上/非 200/0 字节 —— 端点抽风
//     (两个 URL 同为 speed.cloudflare.com, 同生同死)不是节点的错, 不判死;
//   - 慢(仅记 SpeedBPS): 持续有数据但吞吐 < nodeSpeedSlowBPS —— 由选路降权兜底,
//     不判死(实测 p50=36KB/s 的节点跑 LLM 流式完全可用)。
func probeNodeSpeed(client *http.Client) (int64, bool, bool) {
	for _, url := range nodeSpeedTestURLs {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(nodeSpeedBudget*float64(time.Second)))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			continue
		}
		var downloaded int64
		t0 := time.Now()
		stalled := false
		chunk := make([]byte, nodeSpeedChunkSize)
		for {
			// 读前记时刻: 断流判定看单次 Read 本身耗时, 不再依赖与上次 chunk 的
			// 时间差(旧写法先刷新 lastChunk 再判空闲, 差值恒 ≈0, 判定不可达)。
			readStart := time.Now()
			n, err := resp.Body.Read(chunk)
			readDur := time.Since(readStart)
			if n > 0 {
				downloaded += int64(n)
			}
			if readDur.Seconds() > nodeSpeedIdleTimeout {
				stalled = true // 单次 Read 卡 >3s: 不论是否读到数据, 都是断流签名
				break
			}
			if err != nil {
				// 预算 ctx 超时时被卡住的那次 Read 会在上面计入 stalled
				// (5s 预算 >3s 阈值); 快速返回的 err 是预算耗尽/连接结束, 不算断流。
				break
			}
			if time.Since(t0).Seconds() > nodeSpeedBudget {
				break
			}
		}
		resp.Body.Close()
		cancel() // 读完才能取消: 提前 cancel 会让上面的 Read 恒报错, 测速恒为 0 并被误判断流
		elapsed := time.Since(t0).Seconds()
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		if downloaded > 0 {
			return int64(float64(downloaded) / elapsed), stalled, false
		}
		// 200 但 0 字节: 视作该端点失败, 换下一个端点再试
	}
	// 全部端点都失败 = 测速未测(不是断流)
	return 0, false, true
}

// --- MITM + WARP 检测 ---

// probeMITM 经代理访问 gstatic generate_204(HTTPS)并验证 TLS 证书链。
// 证书验证失败 = 中间人劫持; 异常状态码(重定向/403/407/5xx) = 可能被劫持。
func probeMITM(client *http.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), nodeProbeRetryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeMITMURL, nil)
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
