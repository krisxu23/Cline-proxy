package app

// 429 出口轮换的端到端验证(2026-09-23 用户实证的严重 bug)。
//
// 现象: opencode 免费模型撞上出口节点 IP 额度 429 之后, 网关**不换出口**,
// 而是把 429 一路甩回客户端; 客户端重发又落到同一个已耗尽的出口上 ——
// 实测同一个节点被连打 32 次、37 次(跨度 12.5 分钟)。
//
// 两个成因, 本文件各覆盖一条:
//
//	① zen_call.go 的 429 分支在 Retry-After 超过 zenRetryAfterMaxWait 时
//	   直接 return 429, 冷却了出口却**不做换出口重试**。opencode 免费层的
//	   Retry-After 实测 ≈ 到次日 08:00 重置(15h+), 永远超过 1 分钟上限 ——
//	   于是这条分支是**常态**, "换出口重试"从来没发生过。
//	② 首次 attempt 吃共享连接池, 而池里的连接按上游 host 复用 —— 复用直接
//	   绕过出口选择, 并且不触发拨号(→ setReqExit 不写 → reqExit 为空 →
//	   连"该冷却哪个出口"都不知道)。实测 95 次 429 里只有 4 次真的冷却到了
//	   出口。
//
// 用例用两个假 CONNECT 代理当出口、一个假上游当 opencode:
// 上游第 1 次回 429(Retry-After 50000s), 第 2 次回 200。
// 正确行为 = 网关自己换到**另一个**出口把请求打成 200, 而不是把 429 交还。

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConnectProxy 最小 HTTP CONNECT 代理: 记录被使用次数, 隧道直接接到 target。
// 生产里的出口节点就是这一跳 —— 用它当出口可以完全绕开 sing-box 实例,
// 让本用例在任何构建下都能跑(不受 FREE_ROUTER_SKIP_NODEBOX 影响)。
func fakeConnectProxy(t *testing.T, target string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		hits.Add(1)
		up, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			io.Copy(up, client)
			up.Close()
		}()
		io.Copy(client, up)
		client.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withTestExitPool 把出口模式切到代理、出口池设成给定代理列表, 并把 zen
// 上游指到假上游。其余字段沿用当前配置, 避免与本用例无关的默认值漂移。
func withTestExitPool(t *testing.T, proxies []string, baseURL string, retries int) {
	t.Helper()
	orig := getZenConfig()
	cfg := *orig
	cfg.ExitMode = exitModeProxy
	cfg.Proxies = proxies
	cfg.ProxyStrategy = "round_robin"
	cfg.StickySessions = false
	cfg.BaseURL = baseURL
	cfg.BaseURLs = []string{baseURL}
	cfg.Retries = retries
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(orig) })
	// 冷却表/429 计数/熔断窗口都是进程级全局状态, 用例之间必须清干净。
	t.Cleanup(func() {
		zenProxyCooldownsMu.Lock()
		for _, p := range proxies {
			delete(zenProxyCooldowns, zenProxyCooldownKey(p))
			delete(zenProxyQuotaStrikes, zenProxyCooldownKey(p))
		}
		zenProxyCooldownsMu.Unlock()
		zenStateMu.Lock()
		zenFailCount = 0
		zenFailUntil = time.Time{}
		zenProbing = false
		zenStateMu.Unlock()
	})
}

// TestRateLimitedExitRotatesInsteadOfReturning429 是本次 bug 的回归用例。
func TestRateLimitedExitRotatesInsteadOfReturning429(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamHits.Add(1) == 1 {
			// 复刻 opencode 免费层的真实响应: 429 + 超长 Retry-After
			// (额度按出口 IP 计, 到次日 08:00 重置)。
			w.Header().Set("Retry-After", "50000")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded for this ip"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	target := upstream.Listener.Addr().String()

	var hitsA, hitsB atomic.Int32
	proxyA := fakeConnectProxy(t, target, &hitsA)
	proxyB := fakeConnectProxy(t, target, &hitsB)

	withTestExitPool(t, []string{proxyA.URL, proxyB.URL}, upstream.URL, 3)

	// reqExit 平时由日志中间件注入(见 logs.go); 这里手工注入, 否则 setReqExit
	// 无处可写、reqExitKey 恒为空 —— 那正是成因 ② 的表现。
	ctx := context.WithValue(context.Background(), ctxKeyReqExit, &reqExit{})
	params := map[string]any{
		"model":      "mimo-v2.6-flash-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16,
	}

	resp, rateLimited, err := callZenAPI(ctx, params, true)
	if err != nil {
		t.Fatalf("429 后应在预算内换出口重试并成功, got err: %v (上游被请求 %d 次, 出口A %d 次 / 出口B %d 次)",
			err, upstreamHits.Load(), hitsA.Load(), hitsB.Load())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换出口后应拿到 200, got %d", resp.StatusCode)
	}
	if rateLimited != 1 {
		t.Fatalf("应恰好命中 1 次限流, got %d", rateLimited)
	}
	// 核心断言: 第二次尝试必须走**另一个**出口。若仍走同一个出口, 说明出口
	// 轮换没生效(冷却没写进去, 或连接被复用绕过了选路)。
	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("两个出口都应被用到(第二次尝试必须换出口), 实际 出口A %d 次 / 出口B %d 次",
			hitsA.Load(), hitsB.Load())
	}
	if n := hitsA.Load() + hitsB.Load(); n != 2 {
		t.Fatalf("应恰好两次出口尝试(1 次 429 + 1 次成功), got %d", n)
	}
	// 被 429 的那个出口必须已进冷却表, 否则下一次请求还会选中它。
	// 同时: 成功的那次尝试(拨号层回写的真实出口)必须**不是**被冷却的那一个。
	lastExit := reqExitKey(ctx)
	cooled := 0
	zenProxyCooldownsMu.Lock()
	for _, p := range []string{proxyA.URL, proxyB.URL} {
		if until, ok := zenProxyCooldowns[zenProxyCooldownKey(p)]; ok && time.Now().Before(until) {
			cooled++
			if p == lastExit {
				zenProxyCooldownsMu.Unlock()
				t.Fatalf("成功的那次尝试仍走了已冷却的出口 %s —— 出口轮换没生效", describeExitRaw(p))
			}
		}
	}
	zenProxyCooldownsMu.Unlock()
	if cooled != 1 {
		t.Fatalf("应恰好 1 个出口处于冷却中, got %d", cooled)
	}
}

// TestRateLimitedExitWithoutPoolReturns429 兜底路径不能退化成死循环:
// 直连(无出口可冷却)时换出口是空操作, 必须原样交还 429 而不是反复重试。
func TestRateLimitedExitWithoutPoolReturns429(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Retry-After", "50000")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded for this ip"}}`))
	}))
	defer upstream.Close()

	orig := getZenConfig()
	cfg := *orig
	cfg.ExitMode = exitModeDirect
	cfg.Proxies = nil
	cfg.BaseURL = upstream.URL
	cfg.BaseURLs = []string{upstream.URL}
	cfg.Retries = 3
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(orig) })

	ctx := context.WithValue(context.Background(), ctxKeyReqExit, &reqExit{})
	_, rateLimited, err := callZenAPI(ctx, map[string]any{
		"model":      "mimo-v2.6-flash-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16,
	}, true)
	if err == nil {
		t.Fatal("无出口可轮换时必须交还 429, 而不是假装成功")
	}
	if rateLimited != 1 {
		t.Fatalf("应命中 1 次限流, got %d", rateLimited)
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("无出口可轮换时不该重复打上游(换出口是空操作), got %d 次", n)
	}
}
