package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

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
)

var nodeSchemes = map[string]bool{
	"vmess": true, "vless": true, "trojan": true,
	"ss": true, "hy2": true, "hysteria2": true, "tuic": true,
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

// syncNodeBox 按代理列表中的节点链接重建 sing-box 实例(setZenConfig/启动时调用)
func syncNodeBox(proxies []string) {
	nodeMu.Lock()
	defer nodeMu.Unlock()

	seen := map[string]bool{}
	var links []string
	for _, p := range proxies {
		line := strings.TrimSpace(p)
		if !isNodeLink(line) {
			continue
		}
		key := nodeLocalKey(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, line)
	}
	keys := strings.Join(mapKeysSorted(nodePortsKeysOf(links)), "|")
	if keys == nodePortsKeys {
		return
	}

	if nodeBox != nil {
		nodeBox.Close()
		nodeBox = nil
		nodePorts = nil
		nodePortsKeys = ""
	}
	if len(links) == 0 {
		return
	}

	var inbounds, outbounds, rules []map[string]any
	ports := map[string]int{}
	for i, link := range links {
		port, err := freeLocalPort()
		if err != nil {
			log.Printf("  node %d: 分配本地端口失败: %v", i+1, err)
			continue
		}
		ob, err := nodeOutbound(link, fmt.Sprintf("out-%d", i))
		if err != nil {
			log.Printf("  node %d: 解析失败已跳过: %v", i+1, err)
			continue
		}
		tag := ob["tag"].(string)
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": fmt.Sprintf("in-%d", i),
			"listen": "127.0.0.1", "listen_port": port,
		})
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{
			"action": "route", "inbound": []string{fmt.Sprintf("in-%d", i)}, "outbound": tag,
		})
		ports[nodeLocalKey(link)] = port
	}
	if len(outbounds) == 0 {
		return
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})

	cfg := map[string]any{
		"log":       map[string]any{"disabled": true},
		"dns":       map[string]any{"servers": []any{map[string]any{"type": "udp", "tag": "dns-direct", "server": "8.8.8.8"}}},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]any{
			"rules":                    rules,
			"final":                    "direct",
			"default_domain_resolver":  map[string]any{"server": "dns-direct"},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("  nodes: 构建配置失败: %v", err)
		return
	}
	ctx := include.Context(context.Background())
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		log.Printf("  nodes: 配置解析失败: %v", err)
		return
	}
	instance, err := box.New(box.Options{Context: ctx, Options: opts})
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
	nodePortsKeys = keys
	log.Printf("  nodes: %d 个高级节点出口已就绪", len(ports))
}

func nodePortsKeysOf(links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, nodeLocalKey(l))
	}
	return out
}

func mapKeysSorted(m []string) []string {
	s := append([]string(nil), m...)
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
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

// dialNodeProxy 经节点本地 mixed 入站建立 CONNECT 隧道
func dialNodeProxy(ctx context.Context, link, network, addr string) (net.Conn, error) {
	local := nodeLocalAddr(link)
	if local == "" {
		return nil, fmt.Errorf("node outbound not running")
	}
	u, err := url.Parse(local)
	if err != nil {
		return nil, err
	}
	return dialHTTPProxy(ctx, u, network, addr)
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
		ob["tls"] = tlsBlock(sni, insecure, nil)
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
	insecure := q.Get("insecure") == "1" || q.Get("insecure") == "true"
	switch {
	case security == "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return nil, fmt.Errorf("vless: reality missing public key")
		}
		ob["tls"] = tlsBlock(q.Get("sni"), false, map[string]any{
			"utls":    map[string]any{"enabled": true, "fingerprint": orDefault(q.Get("fp"), "chrome")},
			"reality": map[string]any{"enabled": true, "public_key": pbk, "short_id": q.Get("sid")},
		})
	case security == "tls" || (typ == "trojan" && security == ""):
		sni := orDefault(q.Get("sni"), host)
		ob["tls"] = tlsBlock(sni, insecure, map[string]any{
			"utls": map[string]any{"enabled": true, "fingerprint": orDefault(q.Get("fp"), "chrome")},
		})
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

// parseSS ss://base64(method:pass)@host:port#name 或 ss://base64(method:pass@host:port)#name
func parseSS(rest, tag string) (map[string]any, error) {
	if q, err := url.Parse("//" + rest); err == nil && q.Query().Get("plugin") != "" {
		return nil, fmt.Errorf("ss: plugin 不支持")
	}
	var method, password, hostport string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo, err := b64String(rest[:at])
		if err != nil {
			userinfo, err = url.QueryUnescape(rest[:at])
		}
		if err != nil {
			return nil, err
		}
		method, password, _ = strings.Cut(userinfo, ":")
		hostport = rest[at+1:]
	} else {
		whole, err := b64String(rest)
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
	return map[string]any{
		"type": "shadowsocks", "tag": tag, "server": host, "server_port": port,
		"method": method, "password": password,
	}, nil
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
		"tls": tlsBlock(orDefault(q.Get("sni"), host), q.Get("insecure") == "1" || q.Get("insecure") == "true", nil),
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
		"tls": tlsBlock(orDefault(q.Get("sni"), host), q.Get("insecure") == "1" || q.Get("insecure") == "true", nil),
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
