package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var (
	zenHTTPClient  = &http.Client{Transport: buildZenTransport()}
	zenProxyCount  atomic.Uint64
	zenTransportMu sync.Mutex

	zenProxyCooldowns   = map[int]time.Time{} // 代理索引 -> 冷却截止
	zenProxyCooldownsMu sync.Mutex
)

// cooldownZenProxy 标记某出口代理冷却,冷却期内轮询跳过
func cooldownZenProxy(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	if d <= 0 {
		d = 10 * time.Minute
	}
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns[idx] = time.Now().Add(d)
	zenProxyCooldownsMu.Unlock()
}

func zenProxyAvailable(idx int) bool {
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	until, ok := zenProxyCooldowns[idx]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(zenProxyCooldowns, idx)
		return true
	}
	return false
}

func zenProxyCooldownStatus() map[string]string {
	list := effectiveProxyList()
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	out := map[string]string{}
	for idx, until := range zenProxyCooldowns {
		if idx >= 0 && idx < len(list) {
			if time.Now().Before(until) {
				out[list[idx]] = until.Format("15:04:05")
			}
		}
	}
	return out
}

// rebuildZenTransport 代理池或配置变化时重建 zen 上游 HTTP 客户端
func rebuildZenTransport() {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	zenHTTPClient = &http.Client{Transport: buildZenTransport()}
}

func getZenHTTPClient() *http.Client {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	return zenHTTPClient
}

// effectiveProxyList 生效的出口列表: 手动代理/节点 + 订阅解析出的节点
func effectiveProxyList() []string {
	cfg := getZenConfig()
	list := make([]string, 0, len(cfg.Proxies)+len(cfg.Subs)*4)
	list = append(list, cfg.Proxies...)
	list = append(list, subNodeKeysSnapshot()...)
	return list
}

// nodeDialable 节点出口是否已就绪(普通代理恒为可拨)
func nodeDialable(p string) bool {
	if !isNodeLink(p) {
		return true
	}
	return nodeLocalAddr(p) != ""
}

// ============ 全局出口模式 ============
//
// 出口模式作用于整个网关: zen(cline 池 / opencode / 通用 Provider) 的所有上游
// 请求与订阅抓取共用同一个出口决策。direct = 全部直连; proxy = 全部走
// 节点列表里的代理/节点出口(pickZenProxy 内部仍会跳过冷却与不可达节点)。

const (
	exitModeDirect = "direct"
	exitModeProxy  = "proxy"
)

// exitModeDirectNow 当前是否为直连模式。
func exitModeDirectNow() bool {
	return getZenConfig().ExitMode == exitModeDirect
}

// pickZenProxy 按策略选择代理,返回 (代理URL, 索引);无代理返回 ("", -1)。
// 跳过冷却中或未就绪的节点;全部不可用时返回直连。
// 每次调用递增计数,保证 round_robin 顺序与日志索引一致。
func pickZenProxy() (string, int) {
	if exitModeDirectNow() {
		return "", -1
	}
	list := effectiveProxyList()
	n := len(list)
	if n == 0 {
		return "", -1
	}
	idx := int(zenProxyCount.Add(1)-1) % n
	switch getZenConfig().ProxyStrategy {
	case "random":
		idx = int(time.Now().UnixNano() % int64(n))
	case "fill":
		idx = 0
	}
	// 冷却/未就绪/已检测不可达的出口跳过: 线性探测下一个可用代理
	for i := 0; i < n; i++ {
		if zenProxyAvailable(idx) && nodeDialable(list[idx]) && nodeUsable(list[idx]) {
			break
		}
		idx = (idx + 1) % n
	}
	if !nodeDialable(list[idx]) {
		return "", -1
	}
	return list[idx], idx
}

// nodeUsable 已检测为不可达的节点不再参与轮询, 未检测的按可用处理。
// 连通检测结果需要这层过滤才生效: 否则轮询会持续撞上失效节点。
func nodeUsable(p string) bool {
	if !isNodeLink(p) {
		return true
	}
	return healthOf(nodeLocalKey(p)) != "fail"
}

// lastZenProxyIdx 最近一次选择的代理索引(日志用)
func lastZenProxyIdx() int {
	v := int64(zenProxyCount.Load())
	if v <= 0 {
		return -1
	}
	return int((v - 1) % int64(max(1, len(effectiveProxyList()))))
}

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

func buildZenTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	t.DialContext = zenDialContext
	// https 走 HTTP/2 + uTLS Chrome 指纹: 完整浏览器指纹(含 h2),避免 Go 原生指纹被 CF 风控
	t.RegisterProtocol("https", zenHTTP2Transport())
	return t
}

func zenHTTP2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, err := zenDialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				raw.Close()
				return nil, err
			}
			uconn := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, utls.HelloChrome_120)
			if err := uconn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
}

// reqExit 单次请求实际使用的出口, 由日志中间件经 request context 收集
type reqExit struct{ name string }

type ctxKeyReqExitType struct{}

var ctxKeyReqExit = ctxKeyReqExitType{}

// setReqExit 把本次请求选择的出口写入请求上下文(若存在)
func setReqExit(ctx context.Context, proxy string) {
	info, ok := ctx.Value(ctxKeyReqExit).(*reqExit)
	if !ok {
		return
	}
	switch {
	case proxy == "":
		info.name = "直连"
	case isNodeLink(proxy):
		info.name = "节点: " + nodeDisplayName(proxy)
	default:
		info.name = "代理: " + maskProxyURL(proxy)
	}
}

// describeEffectiveExit 当前代理池最近一次轮换命中的出口描述(请求日志回填用)
func describeEffectiveExit() string {
	list := effectiveProxyList()
	if len(list) == 0 {
		return ""
	}
	idx := lastZenProxyIdx()
	if idx < 0 || idx >= len(list) {
		return ""
	}
	p := list[idx]
	switch {
	case p == "":
		return "直连"
	case isNodeLink(p):
		return "节点: " + nodeDisplayName(p)
	default:
		return "代理: " + maskProxyURL(p)
	}
}

func zenDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	modelID, _ := ctx.Value(ctxKeyZenModel).(string)
	p, _ := pickZenProxyForModel(modelID)
	setReqExit(ctx, p)
	if p == "" {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	return dialViaProxy(ctx, p, network, addr)
}

// dialViaProxy 统一拨号:http/https 走 CONNECT,socks5 走 SOCKS5 握手,
// vmess/vless/trojan/ss/hy2/tuic 节点经内嵌 sing-box 的本地入站转发
func dialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	if isNodeLink(raw) {
		return dialNodeProxy(ctx, raw, network, addr)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h", "socks":
		auth := &proxy.Auth{}
		if u.User != nil {
			auth.User = u.User.Username()
			auth.Password, _ = u.User.Password()
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		// 旧接口无 ctx:包装
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// dialHTTPProxy 通过 http(s) 代理建立 CONNECT 隧道
func dialHTTPProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(rawConn); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawConn.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy CONNECT %s: %s %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return rawConn, nil
}
