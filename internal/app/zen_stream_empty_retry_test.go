package app

// 直连 zen 路径的**空回包隐形换出口重试**(2026-09-24 用户实证)。
//
// 现象: 用户换上新版(19da7c7)后, 23:18:16 又撞到一次空包, agent 的工作被打断
// (req_13ebeffd7feb631c, 502 / 2804ms / attempts=1)。
//
// 根因(两条, 本文件各覆盖一条):
//
//	① 那次请求走的是**直连 zen 路径**(模型名 `zen/muse-spark-1.3-contributor-free`
//	   ⇒ proxy.go 落到 routeModel()=="zen" 分支), 而 handleZenChat 里
//	   `st := handleStreamResponseWithUsage(...)` 之后直接 return —— 没有任何
//	   换站重试。候选链那条(routing_dispatch.go 的 delivered>=400)对它是无效的:
//	   resolveRouteChain 只认配置过的别名(本机配置里只有 auto-router)。
//	② 更糟的是: 空流时流处理器**一个字节都不提交**就返回 502, 而调用方直接 return
//	   ⇒ net/http 会替我们发一个 `200 + Content-Length: 0` —— 客户端(agent)收到的
//	   正是那条"空白回复"。所以"没提交"必须显式写成真错误。
//
// 用例用假 CONNECT 代理当出口、假上游当 opencode: 上游第 1 次回"有 finish_reason
// 但零正文"的流(复刻线上形态), 第 2 次回真实内容。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// zenEmptyStreamFrame 复刻线上那次空回包的形态: 带 finish_reason 但零正文。
// finish_reason 必须用**不在合法空终止白名单**里的值("stop"), 否则会被
// streamDeliveryEmpty 判成"合法空回包"而放行 —— 那正是线上没有发生的事。
func zenEmptyStreamFrame(id string) string {
	return "data: " + `{"id":"` + id + `","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

// zenContentStreamFrame 正常有内容的流。
func zenContentStreamFrame(id string) string {
	return "data: " + `{"id":"` + id + `","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

// zenStreamReq 造一个带 reqExit 槽位的请求 —— 生产里由日志中间件注入(见 logs.go),
// 手工注入后 setReqExit 才有处可写, reqExitKey 才能读出"本次真实出口"。
func zenStreamReq() *http.Request {
	ctx := context.WithValue(context.Background(), ctxKeyReqExit, &reqExit{})
	return httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
}

func zenStreamParams() map[string]any {
	return map[string]any{
		"model":      "mimo-v2.6-flash-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16,
	}
}

// TestZenStreamEmptyBodyRetriesAnotherExit 是本次修复的回归用例:
// 直连 zen 路径上的空回包必须**在网关内部**换出口重试, 客户端完全无感。
func TestZenStreamEmptyBodyRetriesAnotherExit(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if upstreamHits.Add(1) == 1 {
			io.WriteString(w, zenEmptyStreamFrame("FAIL-ATTEMPT"))
			return
		}
		io.WriteString(w, zenContentStreamFrame("WIN-ATTEMPT"))
	}))
	defer upstream.Close()

	var hitsA, hitsB atomic.Int32
	proxyA := fakeConnectProxy(t, upstream.Listener.Addr().String(), &hitsA)
	proxyB := fakeConnectProxy(t, upstream.Listener.Addr().String(), &hitsB)
	withTestExitPool(t, []string{proxyA.URL, proxyB.URL}, upstream.URL, 3)

	req := zenStreamReq()
	rec := httptest.NewRecorder()
	st := handleZenStreamChat(rec, req, zenStreamParams(), newZenStatsTracker(zenStatsRecord{Stream: true}), nil)

	if st != http.StatusOK {
		t.Fatalf("空回包换出口后应成功, got %d: %s", st, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("客户端应拿到 200, got %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("换出口后的真实内容必须交付给客户端, 实得:\n%s", out)
	}
	// ★ 失败那一次的字节一个都不许泄漏 —— 响应头未提交, 客户端对它完全无感。
	// 若这条失败, 说明"响应头未提交"这个前提被破坏, 第二次的内容会被接在第一次
	// 已经发出去的流后面(用户明确警告过的顺序陷阱)。
	if strings.Contains(out, "FAIL-ATTEMPT") {
		t.Fatalf("失败那一次的字节不得泄漏给客户端(换出口必须无感), 实得:\n%s", out)
	}
	if n := upstreamHits.Load(); n != 2 {
		t.Fatalf("应恰好两次上游请求(1 次空回包 + 1 次成功), got %d", n)
	}
	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("两次尝试必须走不同出口, 实际 出口A %d 次 / 出口B %d 次", hitsA.Load(), hitsB.Load())
	}
	// 空回包的那个出口必须进冷却表, 否则下一个请求还会选中它; 成功那次走的
	// 不能是被冷却的那一个。
	lastExit := reqExitKey(req.Context())
	cooled := 0
	zenProxyCooldownsMu.Lock()
	for _, p := range []string{proxyA.URL, proxyB.URL} {
		if until, ok := zenProxyCooldowns[zenProxyCooldownKey(p)]; ok && time.Now().Before(until) {
			cooled++
			if p == lastExit {
				zenProxyCooldownsMu.Unlock()
				t.Fatalf("成功的那次尝试仍走了已冷却的出口 %s —— 换出口没生效", describeExitRaw(p))
			}
		}
	}
	zenProxyCooldownsMu.Unlock()
	if cooled != 1 {
		t.Fatalf("应恰好 1 个出口处于冷却中, got %d", cooled)
	}
}

// TestZenStreamEmptyBodyWithoutExitWritesRealError 直连(无出口可换)时:
// 不许反复空转, 但**必须写出真错误** —— 直接 return 会让 net/http 发
// `200 + 空 body`, 那正是用户报的"空包"。
func TestZenStreamEmptyBodyWithoutExitWritesRealError(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, zenEmptyStreamFrame("FAIL-ATTEMPT"))
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

	rec := httptest.NewRecorder()
	st := handleZenStreamChat(rec, zenStreamReq(), zenStreamParams(), newZenStatsTracker(zenStatsRecord{Stream: true}), nil)

	if st != http.StatusBadGateway {
		t.Fatalf("空回包必须交还 502, got %d", st)
	}
	// ★ 核心断言: 必须真的写出 502 + 错误体。只 return 的话 rec.Code 会是 200、
	// body 为空 —— 线上就是这条"200 + 空 body"把 agent 卡住的。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("必须写出真 502(否则 net/http 会发 200 + 空 body), got %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("无出口可换时也必须写出真错误体, 不能留空响应")
	}
	if !strings.Contains(rec.Body.String(), "empty_content") {
		t.Fatalf("错误体应带 empty_content 类型, 实得:\n%s", rec.Body.String())
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("无出口可换时不该反复打上游(换出口是空操作), got %d 次", n)
	}
}

// TestZenStreamEmptyBodyBudgetExhaustedWritesRealError 预算耗尽后:
// 停止换出口 + 写出真错误(用户规格: 超时仍未成功 → 返回一个真错误)。
//
// 预算覆盖为负值即可让"第一次遇到空回包"时就已经超期。刻意不用 0/纳秒:
// Windows 上 time.Now() 的单调时钟有刻度, 同一刻度内 `now.Before(now+1ns)`
// 仍为真, 用例会假通过。
func TestZenStreamEmptyBodyBudgetExhaustedWritesRealError(t *testing.T) {
	old := emptyStreamRetryBudget
	emptyStreamRetryBudget = -time.Second
	t.Cleanup(func() { emptyStreamRetryBudget = old })

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, zenEmptyStreamFrame("FAIL-ATTEMPT"))
	}))
	defer upstream.Close()

	var hitsA, hitsB atomic.Int32
	proxyA := fakeConnectProxy(t, upstream.Listener.Addr().String(), &hitsA)
	proxyB := fakeConnectProxy(t, upstream.Listener.Addr().String(), &hitsB)
	withTestExitPool(t, []string{proxyA.URL, proxyB.URL}, upstream.URL, 3)

	rec := httptest.NewRecorder()
	st := handleZenStreamChat(rec, zenStreamReq(), zenStreamParams(), newZenStatsTracker(zenStatsRecord{Stream: true}), nil)

	if st != http.StatusBadGateway {
		t.Fatalf("预算耗尽必须交还 502, got %d", st)
	}
	if rec.Code != http.StatusBadGateway || rec.Body.Len() == 0 {
		t.Fatalf("预算耗尽也必须写出真错误, got code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("预算已耗尽, 不该再换出口, got %d 次上游请求", n)
	}
}

// TestZenStreamEmptyBodyStopsAtRotationCap 系统性空回包(上游侧坏了, 换哪个出口
// 都一样)时换出口次数必须收敛 —— 否则会在 5 分钟里打上百次上游, 并把上百个健康
// 出口逐个写进 2 分钟冷却表, 后续请求无出口可用。
//
// 池子给足(12 个 > 上限 8)以保证每一轮都真的换到新出口, 请求次数因此可精确断言。
func TestZenStreamEmptyBodyStopsAtRotationCap(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, zenEmptyStreamFrame("FAIL-ATTEMPT"))
	}))
	defer upstream.Close()

	target := upstream.Listener.Addr().String()
	proxies := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		var hits atomic.Int32
		proxies = append(proxies, fakeConnectProxy(t, target, &hits).URL)
	}
	withTestExitPool(t, proxies, upstream.URL, 3)

	rec := httptest.NewRecorder()
	st := handleZenStreamChat(rec, zenStreamReq(), zenStreamParams(), newZenStatsTracker(zenStatsRecord{Stream: true}), nil)

	if st != http.StatusBadGateway {
		t.Fatalf("系统性空回包最终必须交还 502, got %d", st)
	}
	if rec.Code != http.StatusBadGateway || rec.Body.Len() == 0 {
		t.Fatalf("必须写出真错误, got code=%d body=%q", rec.Code, rec.Body.String())
	}
	// 1 次初始 + 8 次换出口 = 9。
	//
	// **这个数字故意写死**(而不是 1+emptyStreamMaxExitRotations): 后者是
	// 同义反复 —— 改常量时它跟着变, 根本抓不到"上限被改坏了"。上限是安全阀
	// (防止 5 分钟里打上百次上游、并冷却整个出口池), 调它必须连带重新确认
	// "8 次 ≈ 20~25 秒仍在 agent 请求超时之内"这条判断, 所以这里就该逼人改测试。
	const wantHits = 9
	if n := upstreamHits.Load(); n != wantHits {
		t.Fatalf("上游请求次数必须收敛到 %d(1 次初始 + %d 次换出口上限), got %d",
			wantHits, emptyStreamMaxExitRotations, n)
	}
}

// TestStreamEmptyFallbackWritesRealError 兜底封装本身的行为锁(供 cline / provider
// 这些"不自己换站重试"的直连路径使用)。
func TestStreamEmptyFallbackWritesRealError(t *testing.T) {
	// 零产出的流: 处理器未提交即返回 502 → 兜底必须写出真 502。
	rec := httptest.NewRecorder()
	up := &http.Response{StatusCode: http.StatusOK, Body: nopCloser{strings.NewReader(zenEmptyStreamFrame("c1"))}}
	if st := handleStreamResponseWithEmptyFallback(rec, up, nil); st != http.StatusBadGateway {
		t.Fatalf("零产出流应返回 502, got %d", st)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("必须写出真 502(否则客户端收到 200 + 空 body), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty_content") {
		t.Fatalf("错误体应带 empty_content 类型, 实得:\n%s", rec.Body.String())
	}

	// 负向对照: 有真实内容的流不得被兜底污染(仍应是 200 + 原样内容)。
	rec2 := httptest.NewRecorder()
	up2 := &http.Response{StatusCode: http.StatusOK, Body: nopCloser{strings.NewReader(zenContentStreamFrame("c2"))}}
	if st := handleStreamResponseWithEmptyFallback(rec2, up2, nil); st != http.StatusOK {
		t.Fatalf("有内容的流应返回 200, got %d", st)
	}
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"content":"ok"`) {
		t.Fatalf("有内容的流必须原样交付, got code=%d body=%q", rec2.Code, rec2.Body.String())
	}
}
