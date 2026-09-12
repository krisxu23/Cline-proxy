package app

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/cline"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
)

// 高级代理节点出口: 代理池直接粘贴 vmess/vless/trojan/ss/hy2/tuic 分享链接,
// 内嵌 sing-box 把每个节点转成本地 mixed 入站端口, 拨号层当作普通 http 代理使用。
// 轮询/冷却/策略逻辑与 http/socks5 代理完全一致。

var (
	nodeMu        sync.Mutex
	nodeBox       *box.Box
	nodePorts     map[string]int // 节点链接(去 # 名称) -> 本地 mixed 端口
	nodePortsKeys string         // 当前运行实例对应的链接集合, 用于配置变化比对
	catchAllPort  int            // 常驻 catch-all 入站的本地端口(0 = 未就绪)
)

// catchAllInTag 常驻兜底入站: 让"任何非节点直选"的网络行为也经 sing-box 出去。
// 它的 route.final 是 direct, 因此出口模式为直连时, 流量依然在 sing-box 内部
// 走 direct 出站(而不是绕开 sing-box 用 Go 原生拨号)。
const catchAllInTag = "in-catchall"

// catchAllLocalAddr catch-all 入站的 SOCKS5 地址; 未就绪返回空串。
func catchAllLocalAddr() string {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if catchAllPort == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", catchAllPort)
}

var nodeSchemes = map[string]bool{
	"vmess": true, "vless": true, "trojan": true,
	"ss": true, "hy2": true, "hysteria2": true, "tuic": true,
	"hysteria": true, "anytls": true,
	"ssh": true, "shadowtls": true, "snell": true,
	"sbox": true, // 订阅提供的原始 sing-box 出站的池内伪链接
}

func isNodeLink(s string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(s), "://")
	return ok && nodeSchemes[scheme]
}

// nodeLocalKey 取去掉 # 名称的链接作为节点唯一键
func nodeLocalKey(link string) string {
	link = strings.TrimSpace(link)
	if i := strings.Index(link, "#"); i >= 0 {
		link = link[:i]
	}
	return link
}

// nodeLocalAddr 节点对应的本地 mixed 入站地址, 未运行返回 ""
func nodeLocalAddr(link string) string {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if p, ok := nodePorts[nodeLocalKey(link)]; ok {
		return fmt.Sprintf("http://127.0.0.1:%d", p)
	}
	return ""
}

// syncNodeBox 按代理列表节点链接 + 订阅解析节点重建 sing-box 实例
// (setZenConfig/订阅刷新/启动时调用)
func syncNodeBox() {
	nodeMu.Lock()
	defer nodeMu.Unlock()

	cfg := getZenConfig()
	var entries []any
	seen := map[string]bool{}
	for _, p := range cfg.Proxies {
		line := strings.TrimSpace(p)
		if !isNodeLink(line) {
			continue
		}
		key := nodeLocalKey(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, line)
	}
	subMu.Lock()
	entries = append(entries, subNodes...)
	subMu.Unlock()

	var keys []string
	for _, e := range entries {
		if k := subEntryKey(e); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	joined := strings.Join(keys, "|")
	// 链接集合没变且实例已在, 无需重建。注意必须同时判断实例存在:
	// 零节点启动时 joined 与初值都是空串, 但实例还没建, 要建出只含
	// catch-all 的实例(直连模式也要经过 sing-box)。
	if joined == nodePortsKeys && nodeBox != nil {
		return
	}

	if nodeBox != nil {
		nodeBox.Close()
		nodeBox = nil
		nodePorts = nil
		nodePortsKeys = ""
		catchAllPort = 0
	}

	var inbounds, outbounds, rules []map[string]any
	ports, inbounds, outbounds, rules, hasMap := buildNodeParts(entries)

	// 常驻 catch-all 入站: 与节点数量无关, 保证"只要网关联网就经过 sing-box"。
	// 零节点时也建实例 —— 直连模式下流量仍走 sing-box 的 direct 出站。
	if cp, err := freeLocalPort(); err == nil {
		catchAllPort = cp
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": catchAllInTag,
			"listen": "127.0.0.1", "listen_port": cp,
		})
	} else {
		log.Printf("  nodes: catch-all 入站端口分配失败: %v", err)
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})

	ctx := include.Context(context.Background())
	instance, err := startNodeInstance(ctx, inbounds, outbounds, rules)
	if err != nil && hasMap {
		// 订阅提供的原始出站可能有个别不合法: 退回仅手动节点链接重建,
		// 避免单个坏节点拖垮全部出口
		log.Printf("  nodes: 全量构建失败(%v), 退回仅手动节点重建", err)
		var p2, inb2, outb2, rules2, _ = buildNodeParts(stringEntries(entries))
		if len(outb2) > 0 {
			// 退回重建同样要保留 catch-all, 否则"全部经 sing-box"在这条路径上失效
			if catchAllPort != 0 {
				inb2 = append(inb2, map[string]any{
					"type": "mixed", "tag": catchAllInTag,
					"listen": "127.0.0.1", "listen_port": catchAllPort,
				})
			}
			outb2 = append(outb2, map[string]any{"type": "direct", "tag": "direct"})
			if inst2, err2 := startNodeInstance(ctx, inb2, outb2, rules2); err2 == nil {
				instance, ports, err = inst2, p2, nil
			} else {
				err = err2
			}
		}
	}
	if err != nil {
		log.Printf("  nodes: 启动失败: %v", err)
		return
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		log.Printf("  nodes: 启动失败: %v", err)
		return
	}
	nodeBox = instance
	nodePorts = ports
	nodePortsKeys = joined
	log.Printf("  nodes: %d 个高级节点出口已就绪", len(ports))
	// 稍后再做连通检测: 上百个节点同时拨号会占满出口与 CPU, 让面板先可用。
	// 在此之前 healthOf 返回 unknown, 出口照常参与轮询(未探测≠不可用)。
	go func() {
		time.Sleep(nodeStartupHealthDelay)
		checkAllNodeHealth()
	}()
}

// sanitizeOutboundTLS 修正出站里会让 sing-box 直接崩溃的 TLS 写法。
//
// sing-box v1.14.0 的 vless / trojan 出站是这么写的:
//
//	if options.TLS != nil {
//	    outbound.tlsConfig, err = tls.NewClientWithOptions(...)  // Enabled=false 时返回 (nil, nil)
//	    outbound.tlsDialer = tls.NewDialer(dialer, outbound.tlsConfig)  // 却无条件建 dialer
//	}
//
// 于是"tls 对象存在但 enabled 不是 true"会构造出 config == nil 的 TLS dialer,
// 第一条连接走到 sing/common/tls.ClientHandshake 就空指针 panic —— 进程直接死
// (vmess 与各 transport 有 nil 保护, 不受影响; vless/trojan 没有)。
//
// 订阅方生成的 sing-box JSON 经常省略 enabled(只给 server_name / utls), 本意
// 显然是启用 TLS, 所以补 enabled=true; 只有显式 false 才把整块删掉(等价语义)。
func sanitizeOutboundTLS(ob map[string]any) {
	raw, ok := ob["tls"]
	if !ok {
		// anytls 在 sing-box 里强制要求 TLS, 缺块直接 "TLS required" 整条剔除;
		// 订阅下发的 raw JSON 经常缺这块(代理软件默认启用所以能连), 按 server 补默认块。
		// ponytail: 仅 anytls(线上实锤), tuic/hy2 等若出现同类剔除再加。
		if typ, _ := ob["type"].(string); typ == "anytls" {
			if server, _ := ob["server"].(string); server != "" {
				ob["tls"] = map[string]any{
					"enabled":     true,
					"server_name": server,
					"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
				}
			}
		}
		return
	}
	if raw == nil {
		// 显式 null: 删掉, 别在配置里留一个 "tls": null
		delete(ob, "tls")
		return
	}
	block, ok := raw.(map[string]any)
	if !ok {
		delete(ob, "tls")
		return
	}
	if enabled, isBool := block["enabled"].(bool); isBool {
		if !enabled {
			delete(ob, "tls")
		}
		return
	}
	// 缺 enabled 字段(或类型不对): 按启用处理, 这是订阅的常见写法
	block["enabled"] = true
}

// ===== 节点连通检测: 经节点出口向 opencode zen / cline 上游发起真实 TLS 连接 =====
var (
	nodeHealthMu      sync.RWMutex
	nodeHealth        = map[string]nodeHealthState{}
	nodeHealthRunMu   sync.Mutex
	nodeHealthRunning bool
)

// nodeStartupHealthDelay 启动后第一次连通检测前的等待。
// 上百个节点同时做 TLS 握手会占满出口与 CPU, 面板在这期间会明显卡顿甚至超时,
// 因此让节点先就绪、面板先可用, 再开始检测。
const nodeStartupHealthDelay = 12 * time.Second

type nodeHealthState struct {
	Ok bool
	At time.Time
}

// healthCheckTargets 连通检测目标: zen 主端点与 cline 上游的 host
func healthCheckTargets() []string {
	var hosts []string
	if raw := zenBaseURLList(getZenConfig()); len(raw) > 0 {
		if u, err := url.Parse(raw[0]); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	if u, err := url.Parse(cline.ClineAPIBase); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// checkNodeHealth 经单个节点出口连到上游: 任一目标 TLS 握手成功即健康
func checkNodeHealth(key string) bool {
	targets := healthCheckTargets()
	if len(targets) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	for _, host := range targets {
		conn, err := dialNodeProxy(ctx, key, "tcp", host+":443")
		if err != nil {
			continue
		}
		hsCtx, hsCancel := context.WithTimeout(ctx, 7*time.Second)
		err = tls.Client(conn, &tls.Config{ServerName: host}).HandshakeContext(hsCtx)
		hsCancel()
		conn.Close()
		if err == nil {
			return true
		}
	}
	return false
}

// checkAllNodeHealth 并发检测全部节点出口(10 并发)。
// 同时只允许一轮: 节点集合在启动阶段会被订阅解析触发多次重建, 每轮重建都会
// 排一次检测, 不设防会让上百个节点的探测成倍重复, 把出口池与 CPU 一起打满
// (面板的"加载失败"与卡顿就是被这种重复风暴拖出来的)。
func checkAllNodeHealth() {
	nodeHealthRunMu.Lock()
	if nodeHealthRunning {
		nodeHealthRunMu.Unlock()
		return
	}
	nodeHealthRunning = true
	nodeHealthRunMu.Unlock()
	defer func() {
		nodeHealthRunMu.Lock()
		nodeHealthRunning = false
		nodeHealthRunMu.Unlock()
	}()

	nodeMu.Lock()
	keys := make([]string, 0, len(nodePorts))
	for k := range nodePorts {
		keys = append(keys, k)
	}
	nodeMu.Unlock()
	if len(keys) == 0 {
		return
	}
	var okCount int32
	var mu sync.Mutex
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok := checkNodeHealth(key)
			mu.Lock()
			if ok {
				okCount++
			}
			mu.Unlock()
			nodeHealthMu.Lock()
			nodeHealth[key] = nodeHealthState{Ok: ok, At: time.Now()}
			nodeHealthMu.Unlock()
		}(k)
	}
	wg.Wait()
	log.Printf("  nodes: 连通检测完成, %d/%d 个出口可达", okCount, len(keys))
	// 连通性刷新后, 同步刷新地区受限模型的节点能力标记, 以及
	// "节点 × 每个上游"的可达性矩阵(后者用于选节点时跳过到该上游不通的节点)。
	probeRegionModelsAsync()
	probeUpstreamMatrixAsync()
}

// startNodeHealthLoop 每 30 分钟复检
func startNodeHealthLoop() {
	t := time.NewTicker(30 * time.Minute)
	go func() {
		for range t.C {
			checkAllNodeHealth()
		}
	}()
}

func stringEntries(entries []any) []any {
	var out []any
	for _, e := range entries {
		if _, ok := e.(string); ok {
			out = append(out, e)
		}
	}
	return out
}

// buildNodeParts 为每个节点条目分配本地端口并生成 sing-box 配置部件
func buildNodeParts(entries []any) (ports map[string]int, inbounds, outbounds, rules []map[string]any, hasMap bool) {
	ports = map[string]int{}
	for i, e := range entries {
		var ob map[string]any
		var key string
		var port int
		var err error
		switch v := e.(type) {
		case string:
			port, err = freeLocalPort()
			if err != nil {
				log.Printf("  node %d: 分配本地端口失败: %v", i+1, err)
				continue
			}
			ob, err = nodeOutbound(v, fmt.Sprintf("out-%d", i))
			if err != nil {
				log.Printf("  node %d: 解析失败已跳过: %v", i+1, err)
				continue
			}
			key = nodeLocalKey(v)
		case map[string]any:
			hasMap = true
			if verr := validateOutboundEntry(v); verr != nil {
				log.Printf("  node %d(%s): 出站无效已剔除: %v", i+1, subEntryKey(v), verr)
				continue
			}
			port, err = freeLocalPort()
			if err != nil {
				log.Printf("  node %d: 分配本地端口失败: %v", i+1, err)
				continue
			}
			cp := map[string]any{"tag": fmt.Sprintf("out-%d", i)}
			for k, val := range v {
				if k != "tag" {
					cp[k] = val
				}
			}
			ob = cp
			key = subEntryKey(v)
		default:
			continue
		}
		tag := ob["tag"].(string)
		sanitizeOutboundTLS(ob)
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": fmt.Sprintf("in-%d", i),
			"listen": "127.0.0.1", "listen_port": port,
		})
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{
			"action": "route", "inbound": []string{fmt.Sprintf("in-%d", i)}, "outbound": tag,
		})
		ports[key] = port
	}
	return ports, inbounds, outbounds, rules, hasMap
}

// startNodeInstance 组装并创建 sing-box 实例(不 Start)
func startNodeInstance(ctx context.Context, inbounds, outbounds, rules []map[string]any) (*box.Box, error) {
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	boxCfg := map[string]any{
		"log":       map[string]any{"disabled": true},
		"dns":       dnsCfg,
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]any{
			"rules":                   rules,
			"final":                   "direct",
			"default_domain_resolver": map[string]any{"server": resolverTag},
		},
	}
	data, err := json.Marshal(boxCfg)
	if err != nil {
		return nil, err
	}
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		return nil, err
	}
	return box.New(box.Options{Context: ctx, Options: opts})
}

// validateOutboundEntry 单独构建校验一个订阅出站, 单个坏节点不影响其他节点
func validateOutboundEntry(ob map[string]any) error {
	entry := map[string]any{"tag": "check"}
	for k, v := range ob {
		if k != "tag" {
			entry[k] = v
		}
	}
	// 按实际运行形态校验: buildNodeParts 落盘前必经 sanitize, 这里先对副本做同样的事,
	// 否则"校验时剔除、运行时能跑"(或反过来), 两边结论打架。
	sanitizeOutboundTLS(entry)
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	boxCfg := map[string]any{
		"log":       map[string]any{"disabled": true},
		"dns":       dnsCfg,
		"outbounds": []any{entry, map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"final":                   "direct",
			"default_domain_resolver": map[string]any{"server": resolverTag},
		},
	}
	data, err := json.Marshal(boxCfg)
	if err != nil {
		return err
	}
	ctx := include.Context(context.Background())
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		return err
	}
	instance, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		return err
	}
	instance.Close()
	return nil
}

// freeLocalPort 预分配一个空闲端口(存在极小竞态窗口, 冲突由 box 启动报错兜底)
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// nodeView 出口节点在管理界面的展示条目
type nodeView struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Running   bool            `json:"running"`
	Health    string          `json:"health"`              // ok / fail / unknown
	Regions   []string        `json:"regions,omitempty"`   // 该出口已验证可用的地区受限模型
	Upstreams map[string]bool `json:"upstreams,omitempty"` // 该出口到各上游的可达性
}

// healthOf 节点最近一次连通检测结果
func healthOf(key string) string {
	nodeHealthMu.RLock()
	defer nodeHealthMu.RUnlock()
	st, ok := nodeHealth[key]
	if !ok {
		return "unknown"
	}
	if st.Ok {
		return "ok"
	}
	return "fail"
}

// nodeLinkScheme 取节点链接/代理的协议名
func nodeLinkScheme(line string) string {
	scheme, _, ok := strings.Cut(line, "://")
	if !ok {
		return "unknown"
	}
	return scheme
}

// nodeDisplayName 节点链接的展示名: #名称(解码) 或 host:port
func nodeDisplayName(line string) string {
	if i := strings.LastIndex(line, "#"); i >= 0 {
		name := line[i+1:]
		if dec, err := url.QueryUnescape(name); err == nil {
			name = dec
		}
		if name != "" {
			return name
		}
	}
	if u, err := url.Parse(line); err == nil && u.Host != "" {
		return u.Host
	}
	if i := strings.Index(line, "://"); i >= 0 {
		return line[i+3:]
	}
	return line
}

// subNodeDisplayName 订阅出站 tag(sub-<序>-名称)的展示名
func subNodeDisplayName(tag string) string {
	parts := strings.SplitN(tag, "-", 3)
	if len(parts) == 3 && parts[2] != "" {
		return parts[2]
	}
	return tag
}

// nodeViews 出口池全量条目: 手动代理/节点 + 订阅节点, 按池内顺序
func nodeViews() []nodeView {
	cfg := getZenConfig()
	out := make([]nodeView, 0, len(cfg.Proxies)+8)
	for _, p := range cfg.Proxies {
		line := strings.TrimSpace(p)
		if line == "" {
			continue
		}
		if isNodeLink(line) {
			key := nodeLocalKey(line)
			out = append(out, nodeView{
				Name: nodeDisplayName(line), Type: nodeLinkScheme(line),
				Source: "手动", Running: nodeLocalAddr(line) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			})
			continue
		}
		out = append(out, nodeView{
			Name: maskProxyURL(line), Type: nodeLinkScheme(line),
			Source: "手动", Running: true, Health: "ok",
		})
	}
	subMu.Lock()
	entries := append([]any(nil), subNodes...)
	subMu.Unlock()
	for _, e := range entries {
		switch v := e.(type) {
		case string:
			key := nodeLocalKey(v)
			out = append(out, nodeView{
				Name: nodeDisplayName(v), Type: nodeLinkScheme(v),
				Source: "订阅", Running: nodeLocalAddr(v) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			})
		case map[string]any:
			tag, _ := v["tag"].(string)
			typ, _ := v["type"].(string)
			key := "sbox://" + tag
			out = append(out, nodeView{
				Name: subNodeDisplayName(tag), Type: typ,
				Source: "订阅", Running: nodeLocalAddr(key) != "", Health: healthOf(key),
				Regions: regionNodeSupport(key), Upstreams: nodeUpstreamSnapshot(key),
			})
		}
	}
	return out
}

// dialNodeProxy 经节点本地 mixed 入站的 SOCKS5 侧建立到 addr 的隧道。
// 入站是 mixed(HTTP + SOCKS5 同端口), 统一走 SOCKS5: 支持 UDP 能力、
// 不在回环上暴露明文 CONNECT 主机名、无 HTTP 报文解析歧义。
func dialNodeProxy(ctx context.Context, link, network, addr string) (net.Conn, error) {
	u, err := socks5ProxyURL(link)
	if err != nil {
		return nil, err
	}
	return dialSOCKS5(ctx, u, network, addr)
}

// ============ 分享链接 → sing-box 出站配置 ============

// nodeOutbound 解析节点链接为 sing-box 出站配置
func nodeOutbound(link, tag string) (map[string]any, error) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(link), "://")
	if i := strings.Index(rest, "#"); i >= 0 {
		rest = rest[:i]
	}
	switch scheme {
	case "vmess":
		return parseVmess(rest, tag)
	case "vless":
		return parseVless(rest, tag)
	case "trojan":
		return parseTrojan(rest, tag)
	case "ss":
		return parseSS(rest, tag)
	case "hy2", "hysteria2":
		return parseHy2(rest, tag)
	case "tuic":
		return parseTuic(rest, tag)
	case "hysteria":
		return parseHysteria(rest, tag)
	case "anytls":
		return parseAnytls(rest, tag)
	case "ssh":
		return parseSSH(rest, tag)
	case "shadowtls":
		return parseShadowtls(rest, tag)
	case "snell":
		return parseSnell(rest, tag)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", scheme)
	}
}

func b64Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("invalid base64")
}

func b64String(s string) (string, error) {
	b, err := b64Decode(s)
	return string(b), err
}

// toInt 接受 json.Number/float64/string 形式的端点字段
func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case string:
		return strconv.Atoi(strings.TrimSpace(x))
	default:
		return 0, fmt.Errorf("not a number: %v", v)
	}
}

func tlsBlock(serverName string, insecure bool, extra map[string]any) map[string]any {
	tls := map[string]any{"enabled": true, "server_name": serverName}
	if insecure {
		tls["insecure"] = true
	}
	for k, v := range extra {
		tls[k] = v
	}
	return tls
}

// isInsecure 分享链接的三种跳过证书校验写法: insecure / allow_insecure / allowInsecure
func isInsecure(q url.Values) bool {
	for _, k := range []string{"insecure", "allow_insecure", "allowInsecure"} {
		switch q.Get(k) {
		case "1", "true":
			return true
		}
	}
	return false
}

// tlsFromQuery 从分享链接 query 构造 TLS 块: sni/insecure/utls 指纹/alpn
func tlsFromQuery(q url.Values, host, defaultFP string) map[string]any {
	tls := map[string]any{"enabled": true, "server_name": orDefault(q.Get("sni"), host)}
	if isInsecure(q) {
		tls["insecure"] = true
	}
	if fp := orDefault(q.Get("fp"), defaultFP); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if alpn := q.Get("alpn"); alpn != "" {
		var list []string
		for _, a := range strings.Split(alpn, ",") {
			if a = strings.TrimSpace(a); a != "" {
				list = append(list, a)
			}
		}
		if len(list) > 0 {
			tls["alpn"] = list
		}
	}
	return tls
}

func transportBlock(net string, q url.Values) (map[string]any, bool) {
	switch net {
	case "ws":
		t := map[string]any{"type": "ws", "path": q.Get("path")}
		if t["path"] == "" {
			t["path"] = "/"
		}
		if h := q.Get("host"); h != "" {
			t["headers"] = map[string]any{"Host": h}
		}
		return t, true
	case "grpc":
		sn := q.Get("serviceName")
		if sn == "" {
			sn = q.Get("path")
		}
		return map[string]any{"type": "grpc", "service_name": sn}, true
	case "http", "h2":
		return map[string]any{"type": "http", "path": q.Get("path"), "host": []string{q.Get("host")}}, true
	}
	return nil, false
}

// parseVmess vmess://base64({v,ps,add,port,id,aid,net,host,path,tls,sni})
func parseVmess(rest, tag string) (map[string]any, error) {
	raw, err := b64Decode(rest)
	if err != nil {
		return nil, err
	}
	info := map[string]any{}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	host, _ := info["add"].(string)
	port, err := toInt(info["port"])
	if err != nil || host == "" {
		return nil, fmt.Errorf("vmess: bad host/port")
	}
	id, _ := info["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("vmess: missing id")
	}
	aid := 0
	if v, ok := info["aid"]; ok {
		aid, _ = toInt(v)
	}
	ob := map[string]any{
		"type": "vmess", "tag": tag, "server": host, "server_port": port,
		"uuid": id, "security": "auto", "alter_id": aid,
	}
	if scy, _ := info["scy"].(string); scy != "" {
		ob["security"] = scy
	}
	net, _ := info["net"].(string)
	if tr, ok := transportBlock(net, url.Values{"path": {fmt.Sprint(info["path"])}, "host": {fmt.Sprint(info["host"])}}); ok {
		ob["transport"] = tr
	}
	if s, _ := info["tls"].(string); s == "tls" {
		sni, _ := info["sni"].(string)
		if sni == "" {
			sni = fmt.Sprint(info["host"])
		}
		if sni == "" {
			sni = host
		}
		insecure := false
		switch fmt.Sprint(info["allowInsecure"]) {
		case "1", "true":
			insecure = true
		}
		extra := map[string]any{}
		if fp, _ := info["fp"].(string); fp != "" {
			extra["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
		}
		ob["tls"] = tlsBlock(sni, insecure, extra)
	}
	return ob, nil
}

// parseVless vless://uuid@host:port?security=&type=&flow=&sni=&pbk=&sid=&fp=...
func parseVless(rest, tag string) (map[string]any, error) {
	return parseUserinfoNode(rest, tag, "vless")
}

// parseTrojan trojan://password@host:port?sni=&type=...
func parseTrojan(rest, tag string) (map[string]any, error) {
	return parseUserinfoNode(rest, tag, "trojan")
}

func parseUserinfoNode(rest, tag, typ string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("%s: bad host/port", typ)
	}
	q := u.Query()
	secret := u.User.Username()
	if secret == "" {
		return nil, fmt.Errorf("%s: missing credential", typ)
	}
	ob := map[string]any{"type": typ, "tag": tag, "server": host, "server_port": port}
	switch typ {
	case "vless":
		ob["uuid"] = secret
		if flow := q.Get("flow"); flow != "" {
			ob["flow"] = flow
		}
	case "trojan":
		ob["password"] = secret
	}
	security := q.Get("security")
	switch {
	case security == "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return nil, fmt.Errorf("vless: reality missing public key")
		}
		tls := tlsFromQuery(q, host, "chrome")
		tls["reality"] = map[string]any{"enabled": true, "public_key": pbk, "short_id": q.Get("sid")}
		ob["tls"] = tls
	case security == "tls" || (typ == "trojan" && security == ""):
		ob["tls"] = tlsFromQuery(q, host, "chrome")
	}
	if tr, ok := transportBlock(q.Get("type"), q); ok {
		ob["transport"] = tr
	}
	return ob, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// parseSS ss://base64(method:pass)@host:port#name 或 ss://base64(method:pass@host:port)#name,
// SIP002 插件参数(v2ray-plugin / obfs-local)按 plugin + plugin_opts 透传
func parseSS(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	base := rest
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}
	var method, password, hostport string
	if at := strings.LastIndex(base, "@"); at >= 0 {
		userinfo, err := b64String(base[:at])
		if err != nil {
			userinfo, err = url.QueryUnescape(base[:at])
		}
		if err != nil {
			return nil, err
		}
		method, password, _ = strings.Cut(userinfo, ":")
		hostport = base[at+1:]
	} else {
		whole, err := b64String(base)
		if err != nil {
			return nil, err
		}
		at := strings.LastIndex(whole, "@")
		if at < 0 {
			return nil, fmt.Errorf("ss: bad format")
		}
		method, password, _ = strings.Cut(whole[:at], ":")
		hostport = whole[at+1:]
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil || method == "" {
		return nil, fmt.Errorf("ss: bad method/host/port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	ob := map[string]any{
		"type": "shadowsocks", "tag": tag, "server": host, "server_port": port,
		"method": method, "password": password,
	}
	// SIP002 插件: plugin=v2ray-plugin;mode=websocket;host=...;tls;... 原样透传
	if pv := q.Get("plugin"); pv != "" {
		parts := strings.Split(pv, ";")
		ob["plugin"] = parts[0]
		if len(parts) > 1 {
			ob["plugin_opts"] = strings.Join(parts[1:], ";")
		}
	}
	return ob, nil
}

// parseHy2 hy2://password@host:port?sni=&insecure=&obfs=&obfs-password=
func parseHy2(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("hy2: bad host/port")
	}
	password, _ := u.User.Password()
	if u.User.Username() != "" && password == "" {
		password = u.User.Username()
	}
	if password == "" {
		return nil, fmt.Errorf("hy2: missing password")
	}
	q := u.Query()
	ob := map[string]any{
		"type": "hysteria2", "tag": tag, "server": host, "server_port": port, "password": password,
		"tls": tlsFromQuery(q, host, ""),
	}
	if obfs := q.Get("obfs"); obfs != "" {
		ob["obfs"] = map[string]any{"type": obfs, "password": q.Get("obfs-password")}
	}
	return ob, nil
}

// parseTuic tuic://uuid:password@host:port?congestion_control=&alpn=&sni=&insecure=
func parseTuic(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("tuic: bad host/port")
	}
	uuid := u.User.Username()
	pass, _ := u.User.Password()
	if uuid == "" {
		return nil, fmt.Errorf("tuic: missing uuid")
	}
	q := u.Query()
	var alpn []string
	for _, a := range strings.Split(q.Get("alpn"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			alpn = append(alpn, a)
		}
	}
	ob := map[string]any{
		"type": "tuic", "tag": tag, "server": host, "server_port": port,
		"uuid": uuid, "password": pass,
		"tls": tlsFromQuery(q, host, ""),
	}
	if cc := q.Get("congestion_control"); cc != "" {
		ob["congestion_control"] = cc
	}
	if rm := q.Get("udp_relay_mode"); rm != "" {
		ob["udp_relay_mode"] = rm
	}
	if len(alpn) > 0 {
		ob["alpn"] = alpn
	}
	return ob, nil
}

// parseHysteria hysteria://auth@host:port?peer=&upmbps=&downmbps=&obfs=&insecure=
// (兼容 auth 放在 query 的 hysteria://host:port?auth= 形式)
func parseHysteria(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("hysteria: bad host/port")
	}
	q := u.Query()
	auth := q.Get("auth")
	if auth == "" && u.User != nil {
		if p, _ := u.User.Password(); p != "" {
			auth = p
		} else {
			auth = u.User.Username()
		}
	}
	if auth == "" {
		return nil, fmt.Errorf("hysteria: missing auth")
	}
	tls := tlsFromQuery(q, host, "")
	if peer := q.Get("peer"); peer != "" {
		tls["server_name"] = peer
	}
	ob := map[string]any{
		"type": "hysteria", "tag": tag, "server": host, "server_port": port,
		"auth_str": auth, "tls": tls,
	}
	// hysteria v1 强制要求带宽参数, 链接缺省时按常见转换器的默认值补齐
	// ponytail: 仅影响发送速率整形, 不影响链路正确性
	if v, err := strconv.Atoi(q.Get("upmbps")); err == nil && v > 0 {
		ob["up_mbps"] = v
	} else if _, ok := ob["up_mbps"]; !ok {
		ob["up_mbps"] = 50
	}
	if v, err := strconv.Atoi(q.Get("downmbps")); err == nil && v > 0 {
		ob["down_mbps"] = v
	} else if _, ok := ob["down_mbps"]; !ok {
		ob["down_mbps"] = 100
	}
	if o := q.Get("obfs"); o != "" {
		ob["obfs"] = o
	}
	return ob, nil
}

// parseAnytls anytls://password@host:port?sni=&insecure=&fp=
func parseAnytls(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("anytls: bad host/port")
	}
	password := u.User.Username()
	if password == "" {
		return nil, fmt.Errorf("anytls: missing password")
	}
	q := u.Query()
	ob := map[string]any{
		"type": "anytls", "tag": tag, "server": host, "server_port": port, "password": password,
		"tls": tlsFromQuery(q, host, "chrome"),
	}
	return ob, nil
}

// parseSSH ssh://user:password@host:port?host_key=a,b (公钥认证场景请用密钥管理, 链接形式仅支持密码)
func parseSSH(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(orDefault(u.Port(), "22"))
	if err != nil || host == "" {
		return nil, fmt.Errorf("ssh: bad host/port")
	}
	user := u.User.Username()
	if user == "" {
		return nil, fmt.Errorf("ssh: missing user")
	}
	pass, _ := u.User.Password()
	ob := map[string]any{"type": "ssh", "tag": tag, "server": host, "server_port": port, "user": user}
	if pass != "" {
		ob["password"] = pass
	}
	if hk := u.Query().Get("host_key"); hk != "" {
		var keys []string
		for _, k := range strings.Split(hk, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			ob["host_key"] = keys
		}
	}
	return ob, nil
}

// parseShadowtls shadowtls://password@host:port?version=3&sni=&insecure=&fp=
func parseShadowtls(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("shadowtls: bad host/port")
	}
	password := u.User.Username()
	if password == "" {
		return nil, fmt.Errorf("shadowtls: missing password")
	}
	q := u.Query()
	version, err := strconv.Atoi(orDefault(q.Get("version"), "3"))
	if err != nil || version < 1 || version > 3 {
		version = 3
	}
	return map[string]any{
		"type": "shadowtls", "tag": tag, "server": host, "server_port": port,
		"version": version, "password": password,
		"tls": tlsFromQuery(q, host, "chrome"),
	}, nil
}

// parseSnell snell://psk@host:port?version=4&obfs=http&obfs-host=
func parseSnell(rest, tag string) (map[string]any, error) {
	u, err := url.Parse("//" + rest)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return nil, fmt.Errorf("snell: bad host/port")
	}
	psk := u.User.Username()
	if psk == "" {
		return nil, fmt.Errorf("snell: missing psk")
	}
	q := u.Query()
	version, err := strconv.Atoi(orDefault(q.Get("version"), "4"))
	if err != nil || (version != 4 && version != 6) {
		version = 4
	}
	ob := map[string]any{
		"type": "snell", "tag": tag, "server": host, "server_port": port,
		"psk": psk, "version": version,
	}
	if o := q.Get("obfs"); o != "" {
		if version == 4 {
			ob["obfs"] = map[string]any{"type": o, "host": q.Get("obfs-host")}
		} else {
			return nil, fmt.Errorf("snell v6 不支持 obfs 参数")
		}
	}
	return ob, nil
}
