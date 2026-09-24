package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chainTestServer 起一个能按模型名决定成败的上游, 用来验证候选链的逐站行为。
//
// 返回值: base url 与"每个模型被请求了几次"的计数。
func chainTestServer(t *testing.T) (string, func(string) int) {
	t.Helper()
	var counts = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		counts[model]++

		switch model {
		case "m-429":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Quota exceeded for metric: x_requests, limit: 5"}}`)
		case "m-500":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"boom"}}`)
		case "m-gone":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"This model is no longer available"}}`)
		case "m-empty":
			// 200 + 空壳: 上游表示"这个模型现在不可用"
			io.WriteString(w, `{"choices":[]}`)
		case "m-tc-noid":
			// 部分免费模型的常见形态: 有 name 没 id, 客户端会整体报错
			io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}]}`)
		case "m-tc-nameless":
			// 无法执行的形态: 全部缺 name 且无正文
			io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_x","type":"function","function":{"arguments":"{}"}}]}}]}`)
		default:
			io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", func(m string) int { return counts[m] }
}

// chainTestSetup 注册两个 provider 与一条路由别名, 出口设为直连(测试不打真实网络)。
func chainTestSetup(t *testing.T, base string, routes map[string][]string) {
	t.Helper()
	withTestConfig(t, &zenConfigData{
		ExitMode: exitModeDirect,
		Routes:   routes,
		Providers: map[string]providerConfig{
			"p1": {BaseURL: base, APIKey: "sk-1"},
			"p2": {BaseURL: base, APIKey: "sk-2"},
		},
	})
	resetCandidateState()
	t.Cleanup(resetCandidateState)
}

func runChain(t *testing.T, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	params := map[string]any{
		"model":      model,
		"stream":     stream,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(16),
	}
	chain, matched, errMsg := resolveRouteChain(model)
	if !matched {
		t.Fatalf("%q must resolve to a chain", model)
	}
	if errMsg != "" {
		t.Fatalf("unexpected resolve error: %s", errMsg)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	handleChainedChat(rec, req, params, chain, model)
	return rec
}

// 第一站限流 -> 换第二站; 且第一站被记入候选层冷却。
func TestChainFailsOverToNextCandidate(t *testing.T) {
	base, calls := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-429", "p2:ok"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from the second hop, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("second hop body must be returned: %s", rec.Body.String())
	}
	if calls("m-429") != 1 || calls("ok") != 1 {
		t.Fatalf("each hop must be tried once: 429=%d ok=%d", calls("m-429"), calls("ok"))
	}
	if why := candidateSkipReason("p1", "m-429"); !strings.Contains(why, classRateLimit) {
		t.Fatalf("failed hop must be cooled as rateLimit, got %q", why)
	}
}

// 冷却中的候选必须直接跳过, 不再打上游。
func TestChainSkipsCooledCandidateWithoutRequest(t *testing.T) {
	base, calls := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-429", "p2:ok"}})

	markCandidateCooldown("p1", "m-429", classRateLimit, "事先冷却")
	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := calls("m-429"); n != 0 {
		t.Fatalf("cooled hop must not be requested, got %d calls", n)
	}
}

// 200 但无内容也算失败: 换下一站, 并把该站记成 empty。
func TestChainTreatsEmpty200AsFailure(t *testing.T) {
	base, calls := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-empty", "p2:ok"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("empty 200 must fail over, got %d: %s", rec.Code, rec.Body.String())
	}
	if why := candidateSkipReason("p1", "m-empty"); !strings.Contains(why, classEmpty) {
		t.Fatalf("empty hop must be cooled as empty, got %q", why)
	}
	if calls("m-empty") != 1 {
		t.Fatalf("empty hop must be tried once, got %d", calls("m-empty"))
	}
}

// 永久性拒绝(已下架)必须转成永久剔除, 而不是短冷却。
func TestChainPermanentRejectionBecomesPermanent(t *testing.T) {
	base, _ := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-gone", "p2:ok"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback to succeed, got %d", rec.Code)
	}
	why := candidateSkipReason("p1", "m-gone")
	if !strings.Contains(why, "永久剔除") {
		t.Fatalf("withdrawn model must be permanently rejected, got %q", why)
	}
}

// 全链失败: 状态码取最后一站, 消息里带上最后一站的错误。
func TestChainAllCandidatesFailPassthroughLastStatus(t *testing.T) {
	base, _ := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-500", "p2:m-gone"}})

	rec := runChain(t, "r", false)
	// 最后一站是 404, 按"上游 4xx 原样透传"返回
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected the last hop status (404), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "all candidates failed") {
		t.Fatalf("error must explain the chain failure: %s", rec.Body.String())
	}
}

// provider 未配置的站点被跳过; 全链都不可用时报 503 而不是 502。
func TestChainNoUsableCandidate(t *testing.T) {
	base, _ := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"ghost:m1"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when nothing can be tried, got %d: %s", rec.Code, rec.Body.String())
	}
}

// 流式: 胜出那一站的 SSE 原样透传(首字节前完成 failover)。
func TestChainStreamPassesThroughWinner(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		calls = append(calls, model)
		if model == "m-429" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	chainTestSetup(t, srv.URL+"/v1", map[string][]string{"r": {"p1:m-429", "p2:ok"}})
	rec := runChain(t, "r", true)

	if !strings.Contains(rec.Body.String(), "po") {
		t.Fatalf("winner stream must reach the client: %s", rec.Body.String())
	}
	if len(calls) != 2 || calls[0] != "m-429" || calls[1] != "ok" {
		t.Fatalf("stream must fail over before the first byte: %v", calls)
	}
}

// 流式空回包必须换站, 且换站对客户端**完全无感**(2026-09-24, 用户规格:
// 上游的错误必须在网关内部消化, 不得让 agent 看到)。
//
// 场景: 第一站回 200 + text/event-stream, **首事件有效**(能过 probeStreamFirstEvent,
// 所以不会走"200 但空流"那条早断分支)但整条流零产出 —— 这正是用户报的"空白回复"形态。
// 此前 routing_dispatch 在 delivered>=400 时无条件 return, 网关内部不换站, 客户端
// 只能自己重试; 现在必须冷却本站 + 换下一站, 并且失败那一站的字节一个都不许泄漏。
func TestChainStreamEmptyBodyFailsOverInvisibly(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		calls = append(calls, model)
		w.Header().Set("Content-Type", "text/event-stream")
		if model == "m-silent" {
			// 首事件"有效"(非 error-only / 非空对象 / 非脚手架形态 → 探测判就绪),
			// 但整条流没有任何用户可见产出 → 流处理器在响应头提交前改判 502。
			io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"mimo-test\"}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	chainTestSetup(t, srv.URL+"/v1", map[string][]string{"r": {"p1:m-silent", "p2:ok"}})
	rec := runChain(t, "r", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("空流站之后换下一站必须成功, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "po") {
		t.Fatalf("胜出那一站的内容必须交付, 实得: %s", out)
	}
	// ★ 关键: 失败那一站的痕迹不得出现在客户端流里 —— 它一个字节都没被交付。
	// 若这条失败, 说明"响应头未提交"这个前提被破坏(比如脚手架帧触发了提交),
	// 第二次的内容就会接在第一次的流后面 —— 用户明确警告过的顺序陷阱。
	if strings.Contains(out, "mimo-test") {
		t.Fatalf("失败那一站的内容不得泄漏给客户端(换站必须无感), 实得: %s", out)
	}
	if len(calls) != 2 || calls[0] != "m-silent" || calls[1] != "ok" {
		t.Fatalf("空流站之后必须换下一站, 实得调用序列: %v", calls)
	}
	if why := candidateSkipReason("p1", "m-silent"); !strings.Contains(why, classEmpty) {
		t.Fatalf("空流站必须按 empty 冷却, got %q", why)
	}
}

// 空流重试预算耗尽后必须返回**真错误**(用户规格: 超时仍未成功 → 返回一个真错误,
// 不是空白、不是无限等)。
//
// 预算覆盖为负值即可让"第一次遇到空流"时就已经超期; 生产值见
// emptyStreamRetryBudget(5 分钟)。刻意不用 0/纳秒: Windows 上 time.Now() 的
// 单调时钟有刻度, 同一刻度内 `now.Before(now+1ns)` 仍为真, 用例会假通过。
func TestChainStreamEmptyBudgetExhaustedReturnsRealError(t *testing.T) {
	old := emptyStreamRetryBudget
	emptyStreamRetryBudget = -time.Second
	t.Cleanup(func() { emptyStreamRetryBudget = old })

	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		calls = append(calls, model)
		w.Header().Set("Content-Type", "text/event-stream")
		// 每一站都是"首事件有效但整条流零产出"。
		io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"mimo-test\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	chainTestSetup(t, srv.URL+"/v1", map[string][]string{"r": {"p1:m-a", "p2:m-b"}})
	rec := runChain(t, "r", true)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("预算耗尽必须返回真 502, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "all candidates failed") {
		t.Fatalf("必须交付真实的错误体(而不是空白), 实得: %s", body)
	}
	// 预算已耗尽 → 不得再换下一站
	if len(calls) != 1 {
		t.Fatalf("预算耗尽后不得继续换站, 实得调用序列: %v", calls)
	}
	// 失败的那一站仍要按 empty 记账(否则它会一直被重复选中)
	if why := candidateSkipReason("p1", "m-a"); !strings.Contains(why, classEmpty) {
		t.Fatalf("预算耗尽的那一站也必须按 empty 冷却, got %q", why)
	}
}

// 内容判定: 只有真正带 content / tool_calls / reasoning 的 200 才算成功。
//
// ★ 2026-09-17 审查 P1-2: 后 5 条是本次新增的边界用例 —— 此前本函数只认
// **字符串** content, 于是带 content[] 的正常回包会被判成空、白白换下一站
// (用户侧表现: 某个模型明明能答, 网关总说它空、老是换模型)。
func TestChatBodyHasContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"正常内容", `{"choices":[{"message":{"content":"hi"}}]}`, true},
		{"工具调用", `{"choices":[{"message":{"content":"","tool_calls":[{"id":"1"}]}}]}`, true},
		{"data 包装", `{"data":{"choices":[{"message":{"content":"hi"}}]}}`, true},
		{"completion 文本", `{"choices":[{"text":"hi"}]}`, true},
		{"空 choices", `{"choices":[]}`, false},
		{"空 content", `{"choices":[{"message":{"content":""}}]}`, false},
		{"空 body", ``, false},
		{"非 JSON", `<html>oops</html>`, false},
		// ── P1-2 新增: 与流式路径统一口径后必须认得的形态 ──
		{"content 数组形态", `{"choices":[{"message":{"content":[{"type":"text","text":"hi"}]}}]}`, true},
		{"content 数组全空", `{"choices":[{"message":{"content":[{"type":"text","text":""}]}}]}`, false},
		{"reasoning", `{"choices":[{"message":{"reasoning_content":"想一下"}}]}`, true},
		{"多 choice 任一有内容", `{"choices":[{"message":{"content":""}},{"message":{"content":"hi"}}]}`, true},
		{"只有 role 骨架", `{"choices":[{"message":{"role":"assistant"}}]}`, false},
	}
	for _, c := range cases {
		if got := chatBodyHasContent([]byte(c.body)); got != c.want {
			t.Errorf("%s: chatBodyHasContent = %v, want %v", c.name, got, c.want)
		}
	}
}
