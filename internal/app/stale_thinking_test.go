package app

// 过期 thinking 剥离重放(stale_thinking.go)的测试。
//
// 三层: 错误识别(两条实证串)、剥离纯函数(claude 形态 + chat 形态 + 幂等)、
// 端到端重放(httptest 上游先 400 后 200, 断言第二发体已剥干净)。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIsStaleThinkingError(t *testing.T) {
	// 两条实证错误串(kimi 带 index 细节, claude 原生短串)。
	if !isStaleThinkingError(400, `{"message":"thinking is enabled but reasoning_content is missing in assistant tool call message at index 1"}`) {
		t.Fatal("应识别 kimi-coding 的 missing thinking 400")
	}
	if !isStaleThinkingError(400, `Invalid signature in thinking block`) {
		t.Fatal("应识别 claude 原生的无效签名 400")
	}
	// 大小写不敏感
	if !isStaleThinkingError(400, "invalid signature IN thinking block") {
		t.Fatal("应大小写不敏感")
	}
	// 非 400 不算 —— 其它状态码出现同样字样是巧合, 修复重放只会掩盖真实错误。
	if isStaleThinkingError(500, "Invalid signature in thinking block") {
		t.Fatal("非 400 不得触发")
	}
	// 无关 400 不算 —— 宽匹配会把普通 400 拉进剥字段重放。
	if isStaleThinkingError(400, `{"message":"tools[0].function must be an object"}`) {
		t.Fatal("无关 400 不得触发")
	}
	// 反向契约错误(The reasoning_content ... must be passed back)绝不能触发:
	// 那条要的是**补**字段, 触发剥离会让它变得更糟。
	if isStaleThinkingError(400, "The reasoning_content in the thinking mode must be passed back to the API.") {
		t.Fatal("反向契约错误不得触发剥离")
	}
}

func TestStripStaleThinking_Claude形态(t *testing.T) {
	params := map[string]any{
		"model":            "claude-sonnet-4",
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": float64(8000)},
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "stale", "signature": "EpwG-fake"},
				map[string]any{"type": "redacted_thinking", "data": "opaque"},
				map[string]any{"type": "text", "text": "keep"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{}},
			}},
			// 病态历史: 整条消息只有过期 thinking 块 → 应整条删除。
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "only"},
			}},
		},
	}
	if !stripStaleThinking(params) {
		t.Fatal("有 thinking 配置与块可剥时应返回 true")
	}
	if _, ok := params["thinking"]; ok {
		t.Error("thinking 配置未被剥掉")
	}
	if _, ok := params["reasoning_effort"]; ok {
		t.Error("reasoning_effort 未被剥掉")
	}
	msgs := params["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("纯 thinking 消息应整条删除, 实得 %d 条", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("应只剩 text + tool_use, 实得 %d 块: %#v", len(blocks), blocks)
	}
	if t0, _ := blocks[0].(map[string]any)["type"].(string); t0 != "text" {
		t.Errorf("第一块应为 text, 实得 %s", t0)
	}
	if t1, _ := blocks[1].(map[string]any)["type"].(string); t1 != "tool_use" {
		t.Errorf("第二块应为 tool_use, 实得 %s", t1)
	}
	// 幂等: 已剥干净再调返回 false —— 调用方据 false 不做无意义重放。
	if stripStaleThinking(params) {
		t.Fatal("第二次调用应返回 false(幂等)")
	}
}

func TestStripStaleThinking_剥assistant字段型reasoning_content(t *testing.T) {
	// 字段型推理(kimi-coding 一类): reasoning_content 是字段、content 是普通
	// 文本 —— assistant 侧必须删掉, 否则转换层在重放体里原样重建 thinking,
	// 重放与原请求相同、再次 400(剥离项 4)。非 assistant 消息与其余字段原样。
	params := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi",
				"reasoning_content": "user 侧不动"},
			map[string]any{"role": "assistant", "content": "ok",
				"reasoning_content": "thinking about it",
				"tool_calls":        []any{map[string]any{"id": "c1", "type": "function"}},
			},
		},
	}
	if !stripStaleThinking(params) {
		t.Fatal("assistant 带字段型 reasoning_content 时应返回 true")
	}
	m := params["messages"].([]any)[1].(map[string]any)
	if _, ok := m["reasoning_content"]; ok {
		t.Fatal("assistant reasoning_content 应被删")
	}
	if m["content"] != "ok" {
		t.Fatalf("content 字符串应原样, got %#v", m["content"])
	}
	if _, ok := m["tool_calls"]; !ok {
		t.Fatal("tool_calls 应保留")
	}
	um := params["messages"].([]any)[0].(map[string]any)
	if _, ok := um["reasoning_content"]; !ok {
		t.Fatal("非 assistant 消息的 reasoning_content 不得被删")
	}
	// 幂等: 已剥干净再调返回 false —— 调用方据 false 不做无意义重放。
	if stripStaleThinking(params) {
		t.Fatal("第二次调用应返回 false(幂等)")
	}
	if stripStaleThinking(nil) {
		t.Fatal("nil 安全且返回 false")
	}
}

// 端到端(APIType=anthropic, chat 出站透传): 首发带外来签名 thinking 块被
// 400 拒绝 → 剥离重放一次 → 第二发体无签名/thinking 块 → 200。
// attempts 初值为 1, 修复重放必须显式顶开预算, 否则根本发不出第二发。
func TestProviderChatReplaysOnceOnStaleThinking(t *testing.T) {
	const fakeSig = "EpwG-FOREIGN-SIGNATURE-marker"
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Invalid signature in thinking block"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	resetKeyHealthState()
	setTestProvider(t, "claude", providerConfig{BaseURL: srv.URL, APIType: "anthropic",
		APIKeys: []providerAPIKey{{Key: "k", Enabled: true}}})
	p := providerByName("claude")

	params := map[string]any{
		"model":      "claude-sonnet-4",
		"max_tokens": float64(64),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(4096)},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "stale", "signature": fakeSig},
				map[string]any{"type": "text", "text": "answer"},
			}},
			map[string]any{"role": "user", "content": "continue"},
		},
	}
	resp, err := p.Chat(context.Background(), params, false)
	if err != nil {
		t.Fatalf("修复重放后应成功: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("重放响应体: %s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("应恰好两发(400 + 修复重放), 实得 %d", len(bodies))
	}
	if !strings.Contains(bodies[0], fakeSig) {
		t.Fatalf("首发应携带外来签名(否则测的不是剥离): %s", bodies[0])
	}
	if strings.Contains(bodies[1], fakeSig) {
		t.Errorf("重放体仍带外来签名: %s", bodies[1])
	}
	if strings.Contains(bodies[1], `"thinking"`) {
		t.Errorf("重放体仍带 thinking 结构: %s", bodies[1])
	}
	if !strings.Contains(bodies[1], "continue") {
		t.Errorf("重放体不应丢掉其余历史: %s", bodies[1])
	}
}

// 端到端(APIFormat=messages, kimi 形态): reasoning_effort 生成 thinking 而
// 历史缺块 → 400 → 剥掉 reasoning_effort 重放 → 第二发转换体不再有 thinking。
func TestProviderChatReplaysOnceOnMissingThinking(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"thinking is enabled but reasoning_content is missing in assistant tool call message at index 1"}}`))
			return
		}
		w.Write([]byte(`{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	resetKeyHealthState()
	setTestProvider(t, "kimx", providerConfig{BaseURL: srv.URL, APIFormat: apiFormatMessages,
		APIKeys: []providerAPIKey{{Key: "k", Enabled: true}}})
	p := providerByName("kimx")

	params := map[string]any{
		"model":            "kimi-k2-thinking",
		"max_tokens":       float64(64),
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "out"},
		},
	}
	resp, err := p.Chat(context.Background(), params, false)
	if err != nil {
		t.Fatalf("修复重放后应成功: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("重放响应体: %s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("应恰好两发, 实得 %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"thinking"`) {
		t.Fatalf("首发应因 reasoning_effort 生成 thinking 配置: %s", bodies[0])
	}
	if strings.Contains(bodies[1], `"thinking"`) {
		t.Errorf("重放体不应再有 thinking 配置: %s", bodies[1])
	}
}

// 作用域锁: OpenAI 形态(chat)上游回同样的 400 字样也**不得**触发剥离重放 ——
// 那条路的契约是反向的, 剥字段只会雪上加霜。断言只发一请求、错误原样交还。
func TestProviderChatDoesNotStripOnOpenAIFormat(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Invalid signature in thinking block"}}`))
	}))
	defer srv.Close()

	resetKeyHealthState()
	setTestProvider(t, "oai", providerConfig{BaseURL: srv.URL,
		APIKeys: []providerAPIKey{{Key: "k", Enabled: true}}})
	p := providerByName("oai")

	params := map[string]any{
		"model":      "m",
		"max_tokens": float64(16),
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	_, err := p.Chat(context.Background(), params, false)
	if err == nil {
		t.Fatal("上游持续 400 时应交还错误")
	}
	pe, ok := err.(*providerError)
	if !ok || pe.Status != http.StatusBadRequest {
		t.Fatalf("应原样交还 400, 实得 %#v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("OpenAI 形态不得触发修复重放, 实发 %d 个请求", n)
	}
}
