package app

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

// 本地出口拨号：节点入站是 sing-box 的 mixed 类型(同一端口同时支持 HTTP 与
// SOCKS5)，这里统一用 SOCKS5 —— 相比 HTTP CONNECT 的优势:
//  1. 支持 UDP ASSOCIATE(HTTP CONNECT 只能转发 TCP), 为将来的 QUIC/HTTP3 上游留路;
//  2. 回环上不再出现明文的 "CONNECT host:443", 本机安全软件不易识别与干扰;
//  3. 无 HTTP 报文解析歧义(大小写、状态行、chunked 等)。

const (
	socks5Version = 0x05

	socks5MethodNoAuth       = 0x00
	socks5MethodUserPass     = 0x02
	socks5MethodNoAcceptable = 0xff

	socks5CmdConnect = 0x01

	socks5AtypIPv4   = 0x01
	socks5AtypDomain = 0x03
	socks5AtypIPv6   = 0x04

	socks5RepSuccess       = 0x00
	socks5HandshakeTimeout = 15 * time.Second
)

// socks5ReplyError 把 SOCKS5 应答码翻译成可读错误。
func socks5ReplyError(rep byte) string {
	switch rep {
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown SOCKS5 reply 0x%02x", rep)
	}
}

// dialSOCKS5 经 SOCKS5 代理(本地 sing-box mixed 入站)建立到 addr 的连接。
//
// proxyURL 支持两种形态: "socks5://127.0.0.1:port" 与
// "socks5://user:pass@127.0.0.1:port"(本地入站通常不需要认证, 认证分支为
// 将来给远端 SOCKS 出口留出能力)。addr 形如 "host:port" —— 域名会被原样
// 交给代理解析(远程解析, 本地不查 DNS)。
func dialSOCKS5(ctx context.Context, proxyURL *url.URL, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: bad target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad target port %q", portStr)
	}

	d := &net.Dialer{Timeout: socks5HandshakeTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	// 握手整体受 ctx 与固定超时双重约束: 卡在半路不能被无限期挂着。
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(socks5HandshakeTimeout))
	}
	if err := socks5Handshake(conn, proxyURL, host, port); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{}) // 握手完成, 交还给调用方管理超时
	return conn, nil
}

// socks5Handshake 完成 方法协商(含可选认证) → CONNECT → 读取应答。
func socks5Handshake(conn net.Conn, proxyURL *url.URL, host string, port int) error {
	var user, pass string
	needAuth := false
	if proxyURL.User != nil {
		user = proxyURL.User.Username()
		pass, _ = proxyURL.User.Password()
		needAuth = user != ""
	}

	// ---- 1. 方法协商 ----
	greeting := []byte{socks5Version, 1, socks5MethodNoAuth}
	if needAuth {
		greeting = []byte{socks5Version, 2, socks5MethodNoAuth, socks5MethodUserPass}
	}
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5: write greeting: %w", err)
	}
	var methodReply [2]byte
	if _, err := io.ReadFull(conn, methodReply[:]); err != nil {
		return fmt.Errorf("socks5: read method: %w", err)
	}
	if methodReply[0] != socks5Version {
		return fmt.Errorf("socks5: unexpected version 0x%02x", methodReply[0])
	}
	chosen := methodReply[1]
	if chosen == socks5MethodNoAcceptable {
		return fmt.Errorf("socks5: no acceptable auth method")
	}
	if chosen == socks5MethodUserPass {
		if err := socks5UserPassAuth(conn, user, pass); err != nil {
			return err
		}
	} else if chosen != socks5MethodNoAuth {
		return fmt.Errorf("socks5: unsupported auth method 0x%02x", chosen)
	}

	// ---- 2. CONNECT 请求: 域名交给代理解析 ----
	req := []byte{socks5Version, socks5CmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, socks5AtypIPv4)
			req = append(req, v4...)
		} else {
			req = append(req, socks5AtypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: target host too long")
		}
		req = append(req, socks5AtypDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write connect: %w", err)
	}

	// ---- 3. 应答 ----
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5: read reply: %w", err)
	}
	if head[0] != socks5Version {
		return fmt.Errorf("socks5: bad reply version 0x%02x", head[0])
	}
	// 无论成败都要把 BND.ADDR/PORT 读完, 否则连接状态错乱。
	if err := discardSocks5Addr(conn, head[3]); err != nil {
		return err
	}
	if head[1] != socks5RepSuccess {
		return fmt.Errorf("socks5: %s", socks5ReplyError(head[1]))
	}
	return nil
}

// socks5UserPassAuth RFC 1929 用户名密码认证。
func socks5UserPassAuth(conn net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return fmt.Errorf("socks5: credential too long")
	}
	buf := []byte{0x01, byte(len(user))}
	buf = append(buf, user...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, pass...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: write auth: %w", err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fmt.Errorf("socks5: read auth reply: %w", err)
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("socks5: authentication failed")
	}
	return nil
}

// discardSocks5Addr 按 ATYP 读掉应答里的地址与端口。
func discardSocks5Addr(conn net.Conn, atyp byte) error {
	var n int
	switch atyp {
	case socks5AtypIPv4:
		n = 4
	case socks5AtypIPv6:
		n = 16
	case socks5AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("socks5: read addr len: %w", err)
		}
		n = int(l[0])
	default:
		return fmt.Errorf("socks5: bad address type 0x%02x", atyp)
	}
	buf := make([]byte, n+2) // 地址 + 2 字节端口
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("socks5: read addr: %w", err)
	}
	return nil
}

// socks5ProxyURL 节点本地入站的 SOCKS5 地址。
// nodeLocalAddr 给的是 "http://127.0.0.1:port", 这里换成 socks5 scheme。
func socks5ProxyURL(link string) (*url.URL, error) {
	local := nodeLocalAddr(link)
	if local == "" {
		return nil, fmt.Errorf("node outbound not running: %s", nodeLocalKey(link))
	}
	u, err := url.Parse(local)
	if err != nil {
		return nil, err
	}
	return &url.URL{Scheme: "socks5", Host: u.Host, User: u.User}, nil
}
