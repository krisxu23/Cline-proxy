package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// 分享链接 → sing-box 出站配置解析(vmess/vless/trojan/ss/hy2/tuic/hysteria/anytls/ssh/shadowtls/snell)。
// 由 nodes.go 拆分而来(P2 结构整理): nodes.go 只保留 Box 生命周期/订阅同步/
// TLS 清洗, 各职责独立成文件便于评审与测试。同包内无需导出调整。
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

// ssMethodCanonical 把常见 SS method 的大小写 / 别名写法归一到 sing-box 接受的规范小写形式。
//
// 订阅聚合源常给大写或别名写法(如 AES-128-CFB / CHACHA20-POLY1305), 不归一会原样透传,
// 而 sing-box 只认小写规范形式, 单个节点就会构建失败、拖垮整组出口。
//
// 表外写法一律原样返回(lookup 命中不了就走 default): 由 buildNodeParts 的逐节点校验决定
// 剔除, 不做猜测性映射(把 rc4 硬改成 gcm 会让节点连得上但握手必然失败, 比剔除更难排查)。
var ssMethodCanonical = map[string]string{
	// chacha20 别名 -> 规范
	"chacha20-poly1305":      "chacha20-ietf-poly1305",
	"chacha20poly1305":       "chacha20-ietf-poly1305",
	"chacha20_poly1305":      "chacha20-ietf-poly1305",
	"chacha20-ietf-poly1305": "chacha20-ietf-poly1305",
	// AES 系列(大写 / 混合写法归一到小写规范)
	"aes-128-cfb": "aes-128-cfb",
	"aes-192-cfb": "aes-192-cfb",
	"aes-256-cfb": "aes-256-cfb",
	"aes-128-gcm": "aes-128-gcm",
	"aes-192-gcm": "aes-192-gcm",
	"aes-256-gcm": "aes-256-gcm",
	"aes-128-ctr": "aes-128-ctr",
	"aes-192-ctr": "aes-192-ctr",
	"aes-256-ctr": "aes-256-ctr",
	// chacha 系列
	"chacha20-ietf":      "chacha20-ietf",
	"xchacha20":          "xchacha20",
	"xchacha20-poly1305": "xchacha20-poly1305",
	// 其余规范写法(大小写归一后原样返回小写)
	"rc4-md5":                       "rc4-md5",
	"none":                          "none",
	"2022-blake3-aes-128-gcm":       "2022-blake3-aes-128-gcm",
	"2022-blake3-aes-256-gcm":       "2022-blake3-aes-256-gcm",
	"2022-blake3-chacha20-poly1305": "2022-blake3-chacha20-poly1305",
}

// normalizeSSMethod 把订阅常见的 SS method 写法大小写不敏感地归一到 sing-box 接受的规范形式。
//
// 实测订阅聚合源大量使用 chacha20-poly1305(v2ray 的写法, 语义即 IETF 变体),
// 而 sing-box 只认 chacha20-ietf-poly1305 —— 不做这一步, 单个 ss 节点就会让整个
// sing-box 实例构建失败, 拖垮全部出口。同样 AES-128-CFB 等大写写法也必须归一到小写。
//
// 表外写法一律原样返回: 由 buildNodeParts 的逐节点校验决定剔除, 不做猜测性
// 映射(把 rc4 硬改成 gcm 会让节点连得上但握手必然失败, 比剔除更难排查)。
func normalizeSSMethod(m string) string {
	method := strings.TrimSpace(m)
	if method == "" {
		return m
	}
	if canon, ok := ssMethodCanonical[strings.ToLower(method)]; ok {
		return canon
	}
	return m
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
		"method": normalizeSSMethod(method), "password": password,
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
