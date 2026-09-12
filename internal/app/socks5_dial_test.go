package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
)

// ---- mock SOCKS5 服务端: 校验客户端握手字节序列 ----

type mockSocks5 struct {
	ln          net.Listener
	requireAuth bool
	wantUser    string
	wantPass    string
	replyCode   byte
	gotTarget   string
}

func newMockSocks5(t *testing.T, requireAuth bool, replyCode byte) *mockSocks5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mockSocks5{ln: ln, requireAuth: requireAuth, replyCode: replyCode, wantUser: "u", wantPass: "p"}
	go m.serve()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *mockSocks5) addr() string { return m.ln.Addr().String() }

func (m *mockSocks5) serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(c)
	}
}

func (m *mockSocks5) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return
	}
	if head[0] != socks5Version {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	hasUserPass := false
	for _, mt := range methods {
		if mt == socks5MethodUserPass {
			hasUserPass = true
		}
	}
	if m.requireAuth {
		if !hasUserPass {
			c.Write([]byte{socks5Version, socks5MethodNoAcceptable})
			return
		}
		c.Write([]byte{socks5Version, socks5MethodUserPass})
		var ah [2]byte
		if _, err := io.ReadFull(c, ah[:]); err != nil {
			return
		}
		u := make([]byte, ah[1])
		io.ReadFull(c, u)
		var pl [1]byte
		io.ReadFull(c, pl[:])
		p := make([]byte, pl[0])
		io.ReadFull(c, p)
		if string(u) != m.wantUser || string(p) != m.wantPass {
			c.Write([]byte{0x01, 0x01})
			return
		}
		c.Write([]byte{0x01, 0x00})
	} else {
		c.Write([]byte{socks5Version, socks5MethodNoAuth})
	}

	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	switch req[3] {
	case socks5AtypIPv4:
		b := make([]byte, 4+2)
		io.ReadFull(c, b)
		m.gotTarget = net.IP(b[:4]).String()
	case socks5AtypDomain:
		var l [1]byte
		io.ReadFull(c, l[:])
		b := make([]byte, int(l[0])+2)
		io.ReadFull(c, b)
		m.gotTarget = string(b[:l[0]])
	case socks5AtypIPv6:
		b := make([]byte, 16+2)
		io.ReadFull(c, b)
	}

	if m.replyCode == 0 {
		c.Write([]byte{socks5Version, 0x00, 0x00, socks5AtypIPv4, 127, 0, 0, 1, 0, 0})
		return
	}
	c.Write([]byte{socks5Version, m.replyCode, 0x00, socks5AtypIPv4, 0, 0, 0, 0, 0, 0})
}

func mustProxyURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 无认证握手 + 域名目标: 域名必须原样传给代理(本地不解析)。
func TestDialSOCKS5NoAuthDomainTarget(t *testing.T) {
	m := newMockSocks5(t, false, 0)
	conn, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://"+m.addr()), "tcp", "api.example.com:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	if m.gotTarget != "api.example.com" {
		t.Fatalf("domain must be passed through unresolved, got %q", m.gotTarget)
	}
}

// IP 目标走 ATYP=IPv4, 不做 DNS。
func TestDialSOCKS5IPTarget(t *testing.T) {
	m := newMockSocks5(t, false, 0)
	conn, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://"+m.addr()), "tcp", "203.0.113.7:8443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	if m.gotTarget != "203.0.113.7" {
		t.Fatalf("IPv4 target must use ATYP=IPv4, got %q", m.gotTarget)
	}
}

// 用户名密码认证分支。
func TestDialSOCKS5UserPassAuth(t *testing.T) {
	m := newMockSocks5(t, true, 0)
	conn, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://u:p@"+m.addr()), "tcp", "x.test:80")
	if err != nil {
		t.Fatalf("dial with auth: %v", err)
	}
	conn.Close()

	if _, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://u:wrong@"+m.addr()), "tcp", "x.test:80"); err == nil ||
		!strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("bad credentials must fail with auth error, got %v", err)
	}
}

// 应答码翻译: host unreachable / connection refused 等要如实报出。
func TestDialSOCKS5ReplyErrors(t *testing.T) {
	cases := []struct {
		rep  byte
		want string
	}{
		{0x04, "host unreachable"},
		{0x05, "connection refused"},
		{0x03, "network unreachable"},
	}
	for _, c := range cases {
		m := newMockSocks5(t, false, c.rep)
		_, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://"+m.addr()), "tcp", "x.test:80")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("rep=0x%02x: want %q, got %v", c.rep, c.want, err)
		}
	}
}

// 非 TCP 网络(如 udp)当前必须明确拒绝, 而不是静默走错路径。
func TestDialSOCKS5RejectsNonTCP(t *testing.T) {
	m := newMockSocks5(t, false, 0)
	if _, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://"+m.addr()), "udp", "x.test:53"); err == nil {
		t.Fatal("udp must be rejected explicitly")
	}
}

// 目标地址格式错误要立刻报错, 不能发出半个包。
func TestDialSOCKS5BadTarget(t *testing.T) {
	m := newMockSocks5(t, false, 0)
	for _, bad := range []string{"no-port", "host:notanumber", "host:0", "host:70000"} {
		if _, err := dialSOCKS5(context.Background(), mustProxyURL(t, "socks5://"+m.addr()), "tcp", bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// 真实 sing-box 集成: mixed 入站 + direct 出站, 用本地 SOCKS5 拨号访问本地 HTTP 服务。
// 证明 mixed 入站的 SOCKS5 侧确实可用, 且不依赖外部网络。
func TestDialSOCKS5ThroughRealSingBox(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	boxCfg := map[string]any{
		"log": map[string]any{"disabled": true},
		"inbounds": []any{map[string]any{
			"type": "mixed", "tag": "in-test", "listen": "127.0.0.1", "listen_port": port,
		}},
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"final": "direct"},
	}
	data, _ := sjson.Marshal(boxCfg)
	ctx := include.Context(context.Background())
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		t.Fatalf("sing-box options: %v", err)
	}
	b, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		t.Fatalf("sing-box start: %v", err)
	}
	defer b.Close()
	// box.New 只构建实例, 监听由 Start() 建立(与 nodes.go 的用法一致)。
	if err := b.Start(); err != nil {
		t.Fatalf("sing-box Start: %v", err)
	}

	target := strings.TrimPrefix(upstream.URL, "http://")
	proxyURL := mustProxyURL(t, "socks5://127.0.0.1:"+strconv.Itoa(port))
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialSOCKS5(ctx, proxyURL, network, addr)
			},
		},
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get("http://" + target + "/")
	if err != nil {
		t.Fatalf("request through sing-box mixed inbound: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}
