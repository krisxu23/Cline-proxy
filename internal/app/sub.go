package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"cline-go-proxy/internal/kit"
	"gopkg.in/yaml.v3"
)

// 订阅链接: 代理池的「订阅链接」里填订阅地址, 定期抓取并展开为节点,
// 与手动代理合并后一起进入轮询。支持三种内容格式:
// sing-box JSON 配置(outbounds) / Clash YAML(proxies) / base64 或明文节点链接列表。

const subRefreshInterval = 6 * time.Hour

var (
	subMu       sync.Mutex
	subNodes    []any                // 解析后的节点: 节点链接 string 或 sing-box 出站 map
	subNodeKeys []string             // 与 subNodes 一一对应的池内键(节点链接或 sbox://tag)
	subStatus   = map[string]string{} // 订阅 URL -> 最近抓取结果
)

func subCacheFile() string { return kit.ResolveDataPath("subs_cache.json") }

// subNodeKeysSnapshot 当前订阅节点的池内键
func subNodeKeysSnapshot() []string {
	subMu.Lock()
	defer subMu.Unlock()
	return append([]string(nil), subNodeKeys...)
}

func subStatusSnapshot() map[string]string {
	subMu.Lock()
	defer subMu.Unlock()
	out := make(map[string]string, len(subStatus))
	for k, v := range subStatus {
		out[k] = v
	}
	return out
}

func saveSubCacheLocked() {
	b, err := json.Marshal(map[string]any{"nodes": subNodes})
	if err == nil {
		os.WriteFile(subCacheFile(), b, 0600)
	}
}

// loadSubCache 启动时恢复上次解析的订阅节点, 无需等待网络
func loadSubCache() {
	b, err := os.ReadFile(subCacheFile())
	if err != nil {
		return
	}
	var c struct {
		Nodes []any `json:"nodes"`
	}
	if json.Unmarshal(b, &c) == nil && len(c.Nodes) > 0 {
		subMu.Lock()
		subNodes = c.Nodes
		rebuildSubKeysLocked()
		subMu.Unlock()
		log.Printf("  订阅缓存: %d 个节点已恢复", len(subNodes))
	}
}

func rebuildSubKeysLocked() {
	subNodeKeys = make([]string, 0, len(subNodes))
	for _, e := range subNodes {
		subNodeKeys = append(subNodeKeys, subEntryKey(e))
	}
}

// subEntryKey 节点条目在代理池中的键: 链接原文(去名称)或 sbox://tag
func subEntryKey(e any) string {
	switch v := e.(type) {
	case string:
		return nodeLocalKey(v)
	case map[string]any:
		tag, _ := v["tag"].(string)
		return "sbox://" + tag
	default:
		return ""
	}
}

// resolveSubscriptions 抓取全部订阅并重建节点池
func resolveSubscriptions(urls []string) {
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		subMu.Lock()
		subNodes = nil
		subNodeKeys = nil
		saveSubCacheLocked()
		subMu.Unlock()
		syncNodeBox()
		return
	}
	var merged []any
	for _, u := range clean {
		nodes, err := fetchSubscription(u)
		if err != nil {
			log.Printf("  订阅 %s 抓取失败: %v", u, err)
			subMu.Lock()
			subStatus[u] = "❌ 抓取失败: " + err.Error()
			subMu.Unlock()
			continue
		}
		log.Printf("  订阅 %s: 解析出 %d 个节点", u, len(nodes))
		subMu.Lock()
		subStatus[u] = fmt.Sprintf("✅ %s · %d 节点", time.Now().Format("01-02 15:04"), len(nodes))
		subMu.Unlock()
		merged = append(merged, nodes...)
	}
	subMu.Lock()
	subNodes = merged
	rebuildSubKeysLocked()
	saveSubCacheLocked()
	subMu.Unlock()
	syncNodeBox()
}

// refreshSubsLoop 后台定期刷新订阅
func refreshSubsLoop(subs []string) {
	resolveSubscriptions(subs)
	t := time.NewTicker(subRefreshInterval)
	defer t.Stop()
	for range t.C {
		cfg := getZenConfig()
		if len(cfg.Subs) > 0 {
			resolveSubscriptions(cfg.Subs)
		}
	}
}

func fetchSubscription(u string) ([]any, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 常见订阅端按 UA 分发格式: clash UA 得 YAML, 我们的解析器三种格式通吃
	req.Header.Set("User-Agent", "clash.meta/1.18.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseSubContent(string(body))
}

// parseSubContent 识别订阅内容格式并解析为节点列表
func parseSubContent(body string) ([]any, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}
	if strings.HasPrefix(trimmed, "{") {
		return parseSingBoxSub(trimmed)
	}
	if strings.Contains(trimmed, "\nproxies:") || strings.HasPrefix(trimmed, "proxies:") {
		if nodes, err := parseClashSub(trimmed); err == nil && len(nodes) > 0 {
			return nodes, nil
		}
	}
	text := trimmed
	if !strings.Contains(text, "://") {
		if dec, err := b64Decode(strings.Join(strings.Fields(text), "")); err == nil {
			text = string(dec)
		}
	}
	var nodes []any
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "://") || strings.Contains(line, "<") {
			continue
		}
		// 只保留节点/代理形态的行(仅 authority, 无资源路径), 排除普通网页链接
		if !isNodeOrProxyLine(line) {
			continue
		}
		key := subEntryKey(line)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		nodes = append(nodes, line)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("未识别的订阅内容格式")
	}
	return nodes, nil
}

// isNodeOrProxyLine 判断行是否为节点/代理链接: scheme 已知且不含资源路径
func isNodeOrProxyLine(line string) bool {
	scheme, rest, ok := strings.Cut(line, "://")
	if !ok {
		return false
	}
	if !nodeSchemes[scheme] && scheme != "http" && scheme != "https" && scheme != "socks5" && scheme != "socks5h" {
		return false
	}
	if i := strings.IndexAny(rest, "#"); i >= 0 {
		rest = rest[:i]
	}
	return !strings.Contains(rest, "/")
}

// parseSingBoxSub sing-box JSON 配置: 取可用出站(跳过 direct/block/分组等)
func parseSingBoxSub(body string) ([]any, error) {
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return nil, err
	}
	skip := map[string]bool{"direct": true, "block": true, "dns": true, "selector": true, "urltest": true, "group": true}
	var nodes []any
	used := map[string]bool{}
	for _, ob := range cfg.Outbounds {
		typ, _ := ob["type"].(string)
		if typ == "" || skip[typ] {
			continue
		}
		name, _ := ob["tag"].(string)
		if name == "" {
			name = fmt.Sprint(ob["server"])
		}
		tag := "sub-" + sanitizeNodeName(name)
		for used[tag] {
			tag += "x"
		}
		used[tag] = true
		ob["tag"] = tag
		nodes = append(nodes, ob)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("sing-box 配置中没有可用出站")
	}
	return nodes, nil
}

func sanitizeNodeName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// parseClashSub Clash YAML proxies → sing-box 出站
func parseClashSub(body string) ([]any, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, err
	}
	var nodes []any
	used := map[string]bool{}
	for i, m := range doc.Proxies {
		ob, err := clashToOutbound(m, i)
		if err != nil {
			log.Printf("  订阅: 跳过 Clash 节点 %v: %v", m["name"], err)
			continue
		}
		tag := ob["tag"].(string)
		for used[tag] {
			tag += "x"
		}
		used[tag] = true
		ob["tag"] = tag
		nodes = append(nodes, ob)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("Clash 配置中没有可转换的节点")
	}
	return nodes, nil
}

// clashToOutbound 常见 Clash 节点字段 → sing-box 出站配置
func clashToOutbound(m map[string]any, i int) (map[string]any, error) {
	typ, _ := m["type"].(string)
	server, _ := m["server"].(string)
	port, err := yInt(m["port"])
	if err != nil || server == "" {
		return nil, fmt.Errorf("bad server/port")
	}
	name, _ := m["name"].(string)
	tag := fmt.Sprintf("sub-%d-%s", i, sanitizeNodeName(name))
	ob := map[string]any{"tag": tag, "server": server, "server_port": port}
	tls := clashTLS(m, server, typ)
	switch typ {
	case "ss":
		cipher, _ := m["cipher"].(string)
		password, _ := m["password"].(string)
		ob["type"] = "shadowsocks"
		ob["method"] = cipher
		ob["password"] = password
		if pname, _ := m["plugin"].(string); pname != "" {
			ob["plugin"] = clashPluginName(pname)
			ob["plugin_opts"] = serializeClashPluginOpts(m["plugin-opts"])
		}
	case "vmess":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "vmess"
		ob["uuid"] = uuid
		ob["alter_id"] = yIntOr(m["alterId"], 0)
		ob["security"] = yStrOr(m["cipher"], "auto")
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "vless":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "vless"
		ob["uuid"] = uuid
		if flow, _ := m["flow"].(string); flow != "" {
			ob["flow"] = flow
		}
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "trojan":
		password, _ := m["password"].(string)
		ob["type"] = "trojan"
		ob["password"] = password
		if tls != nil {
			ob["tls"] = tls
		}
		if tr := clashTransport(m); tr != nil {
			ob["transport"] = tr
		}
	case "hysteria":
		auth := yStrOr(m["auth-str"], yStrOr(m["auth_str"], yStrOr(m["auth"], "")))
		if auth == "" {
			return nil, fmt.Errorf("missing auth")
		}
		ob["type"] = "hysteria"
		ob["auth_str"] = auth
		if v, err := yInt(m["up"]); err == nil && v > 0 {
			ob["up_mbps"] = v
		} else {
			ob["up_mbps"] = 50
		}
		if v, err := yInt(m["down"]); err == nil && v > 0 {
			ob["down_mbps"] = v
		} else {
			ob["down_mbps"] = 100
		}
		if o, _ := m["obfs"].(string); o != "" {
			ob["obfs"] = o
		}
		if tls != nil {
			ob["tls"] = tls
		}
	case "hysteria2":
		password, _ := m["password"].(string)
		ob["type"] = "hysteria2"
		ob["password"] = password
		if o, _ := m["obfs"].(string); o != "" {
			ob["obfs"] = map[string]any{"type": o, "password": yStrOr(m["obfs-password"], "")}
		}
		if tls != nil {
			ob["tls"] = tls
		}
	case "tuic":
		uuid, _ := m["uuid"].(string)
		ob["type"] = "tuic"
		ob["uuid"] = uuid
		ob["password"] = yStrOr(m["password"], "")
		if cc, _ := m["congestion-controller"].(string); cc != "" {
			ob["congestion_control"] = cc
		}
		if alpn := yStrList(m["alpn"]); len(alpn) > 0 {
			ob["alpn"] = alpn
		}
		if tls != nil {
			ob["tls"] = tls
		}
	case "socks5":
		ob["type"] = "socks"
		ob["version"] = "5"
		ob["username"] = yStrOr(m["username"], "")
		ob["password"] = yStrOr(m["password"], "")
	case "http":
		ob["type"] = "http"
		ob["username"] = yStrOr(m["username"], "")
		ob["password"] = yStrOr(m["password"], "")
		if b, _ := m["tls"].(bool); b {
			if tls != nil {
				ob["tls"] = tls
			} else {
				ob["tls"] = map[string]any{"enabled": true, "server_name": server}
			}
		}
	default:
		return nil, fmt.Errorf("不支持的类型 %q", typ)
	}
	return ob, nil
}

// clashTLS Clash 节点的 TLS 相关字段 → sing-box tls 块
func clashTLS(m map[string]any, server, typ string) map[string]any {
	b, _ := m["tls"].(bool)
	reality, _ := m["reality-opts"].(map[string]any)
	// trojan/hysteria2/tuic 协议恒为 TLS, Clash 配置中没有 tls 字段
	tlsAlways := typ == "trojan" || typ == "hysteria2" || typ == "tuic"
	if !b && reality == nil && !tlsAlways {
		return nil
	}
	sni := yStrOr(m["servername"], yStrOr(m["sni"], server))
	tls := map[string]any{"enabled": true, "server_name": sni}
	if sb, _ := m["skip-cert-verify"].(bool); sb {
		tls["insecure"] = true
	}
	if fp, _ := m["client-fingerprint"].(string); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if alpn := yStrList(m["alpn"]); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if reality != nil {
		tls["reality"] = map[string]any{
			"enabled": true,
			"public_key": yStrOr(reality["public-key"], ""),
			"short_id":   yStrOr(reality["short-id"], ""),
		}
	}
	return tls
}

// clashTransport Clash network/ws-opts/grpc-opts → sing-box transport
func clashTransport(m map[string]any) map[string]any {
	network, _ := m["network"].(string)
	switch network {
	case "ws":
		t := map[string]any{"type": "ws", "path": "/"}
		if w, ok := m["ws-opts"].(map[string]any); ok {
			if p, _ := w["path"].(string); p != "" {
				t["path"] = p
			}
			if h, ok := w["headers"].(map[string]any); ok {
				if host, _ := h["Host"].(string); host != "" {
					t["headers"] = map[string]any{"Host": host}
				}
			}
		}
		return t
	case "grpc":
		svc := ""
		if g, ok := m["grpc-opts"].(map[string]any); ok {
			svc, _ = g["grpc-service-name"].(string)
		}
		return map[string]any{"type": "grpc", "service_name": svc}
	case "h2", "http":
		t := map[string]any{"type": "http"}
		if h, ok := m["h2-opts"].(map[string]any); ok {
			if host := yStrList(h["host"]); len(host) > 0 {
				t["host"] = host
			}
		}
		return t
	}
	return nil
}

func clashPluginName(n string) string {
	switch n {
	case "obfs", "simple-obfs":
		return "obfs-local"
	default:
		return n
	}
}

// serializeClashPluginOpts Clash plugin-opts map → SIP002 k=v; 字符串
func serializeClashPluginOpts(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch val := m[k].(type) {
		case bool:
			if val {
				parts = append(parts, k)
			}
		case string:
			if val != "" {
				parts = append(parts, k+"="+val)
			}
		default:
			parts = append(parts, k+"="+fmt.Sprint(val))
		}
	}
	return strings.Join(parts, ";")
}

func yStrOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func yIntOr(v any, def int) int {
	if n, err := yInt(v); err == nil {
		return n
	}
	return def
}

func yInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(x))
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func yBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func yStrList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, it := range list {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
