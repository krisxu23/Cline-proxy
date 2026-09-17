package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// asMap 是测试断言的辅助函数: 把 any 安全地转成 map[string]any。
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", v)
	}
	return m
}

// equalAny 对 map / slice / 标量做浅层-递归相等比较, 用于透传断言。
func equalAny(a, b any) bool {
	// 实现常把某些字段以 json.RawMessage 原样透传(例如 anthropicToOpenAI 的
	// tool_choice), 语义与 map 完全一致但 Go 类型不同。不先归一化就会得到
	// "tool_choice wrong: json.RawMessage{0x7b,...}" 这类假失败。
	if raw, ok := a.(json.RawMessage); ok {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return false
		}
		return equalAny(v, b)
	}
	if raw, ok := b.(json.RawMessage); ok {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return false
		}
		return equalAny(a, v)
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v1 := range av {
			v2, ok := bv[k]
			if !ok || !equalAny(v1, v2) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalAny(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// ============================================================================
// responsesToChat
// ============================================================================

func TestResponsesToChatModelPassthrough(t *testing.T) {
	// 最关键的一条: model 必须原样透传到 chat 请求体。
	// 这是被审计证明「完全无防护」的回归点。
	cases := []struct {
		name  string
		model string
	}{
		{"gpt-style", "gpt-4o-mini"},
		{"claude-style", "claude-3-5-sonnet"},
		{"provider-prefix", "zen:deepseek-v4-flash-free"},
		{"weird-chars", "model___LEAD_VERIFY_BROKEN___"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := responsesToChat(map[string]any{"model": c.model, "input": "hi"})
			if out["model"] != c.model {
				t.Fatalf("model passthrough broken: got %v, want %q", out["model"], c.model)
			}
		})
	}
}

func TestResponsesToChatMaxOutputTokens(t *testing.T) {
	// max_output_tokens -> max_tokens, 且类型必须是 int 而非 float64。
	out := responsesToChat(map[string]any{
		"model":             "m",
		"max_output_tokens": float64(2048),
		"input":             "hi",
	})
	mt, ok := out["max_tokens"]
	if !ok {
		t.Fatalf("max_tokens missing from output")
	}
	if _, isInt := mt.(int); !isInt {
		t.Fatalf("max_tokens should be int, got %T (%v)", mt, mt)
	}
	if mt.(int) != 2048 {
		t.Fatalf("max_tokens value wrong: got %d want 2048", mt.(int))
	}
}

func TestResponsesToChatInstructions(t *testing.T) {
	// instructions 非空 -> messages[0] 是 system 指令, 其余来自 input
	t.Run("with instructions", func(t *testing.T) {
		out := responsesToChat(map[string]any{
			"model":        "m",
			"instructions": "be terse",
			"input":        "hello",
		})
		msgs, ok := out["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("expected >=2 messages, got %#v", out["messages"])
		}
		sys := asMap(t, msgs[0])
		if sys["role"] != "system" {
			t.Fatalf("messages[0].role = %v, want system", sys["role"])
		}
		if sys["content"] != "be terse" {
			t.Fatalf("messages[0].content = %v, want %q", sys["content"], "be terse")
		}
		user := asMap(t, msgs[1])
		if user["role"] != "user" || user["content"] != "hello" {
			t.Fatalf("messages[1] wrong: %#v", user)
		}
	})

	// instructions 缺失/空串 -> messages 完全来自 input
	t.Run("no instructions", func(t *testing.T) {
		out := responsesToChat(map[string]any{"model": "m", "input": "hello"})
		msgs := out["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("expected exactly 1 message, got %d", len(msgs))
		}
		user := asMap(t, msgs[0])
		if user["role"] != "user" || user["content"] != "hello" {
			t.Fatalf("expected user/hello, got %#v", user)
		}
	})
	t.Run("empty instructions", func(t *testing.T) {
		out := responsesToChat(map[string]any{"model": "m", "instructions": "", "input": "hi"})
		msgs := out["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("expected exactly 1 message, got %d", len(msgs))
		}
		if asMap(t, msgs[0])["role"] != "user" {
			t.Fatalf("first message should be user when instructions empty")
		}
	})
}

func TestResponsesToChatPassThrough(t *testing.T) {
	// stream / temperature / top_p / stop / seed / user / metadata / logit_bias 透传
	in := map[string]any{
		"model":       "m",
		"input":       "hi",
		"stream":      true,
		"temperature": float64(0.7),
		"top_p":       float64(0.9),
		"stop":        []any{"\n", "STOP"},
		"seed":        float64(42),
		"user":        "u1",
		"metadata":    map[string]any{"k": "v"},
		"logit_bias":  map[string]any{"123": float64(-1)},
	}
	out := responsesToChat(in)
	checks := map[string]any{
		"stream":      true,
		"temperature": float64(0.7),
		"top_p":       float64(0.9),
		"stop":        []any{"\n", "STOP"},
		"seed":        float64(42),
		"user":        "u1",
		"metadata":    map[string]any{"k": "v"},
		"logit_bias":  map[string]any{"123": float64(-1)},
	}
	for k, want := range checks {
		got, ok := out[k]
		if !ok {
			t.Fatalf("missing passthrough key %q", k)
		}
		if !equalAny(got, want) {
			t.Fatalf("passthrough key %q: got %#v want %#v", k, got, want)
		}
	}
}

func TestResponsesToChatTools(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "function", "name": "f1", "description": "d1", "parameters": map[string]any{"type": "object"}},
		},
	}
	out := responsesToChat(in)
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not mapped: %#v", out["tools"])
	}
	fn := asMap(t, asMap(t, tools[0])["function"])
	if fn["name"] != "f1" {
		t.Fatalf("tool name = %v, want f1", fn["name"])
	}
	if fn["description"] != "d1" {
		t.Fatalf("tool description = %v, want d1", fn["description"])
	}
	if _, hasParams := fn["parameters"]; !hasParams {
		t.Fatalf("tool parameters missing")
	}
}

func TestResponsesToChatToolChoice(t *testing.T) {
	tc := map[string]any{"type": "function", "function": map[string]any{"name": "f1"}}
	out := responsesToChat(map[string]any{"model": "m", "input": "hi", "tool_choice": tc})
	if !equalAny(out["tool_choice"], tc) {
		t.Fatalf("tool_choice not passed through: %#v", out["tool_choice"])
	}
}

// ============================================================================
// responsesInputToMessages
// ============================================================================

func TestResponsesInputToMessages(t *testing.T) {
	t.Run("string input", func(t *testing.T) {
		msgs := responsesInputToMessages("just text")
		if len(msgs) != 1 {
			t.Fatalf("expected 1 msg, got %d", len(msgs))
		}
		m := asMap(t, msgs[0])
		if m["role"] != "user" || m["content"] != "just text" {
			t.Fatalf("got %#v", m)
		}
	})

	t.Run("array with message and input_text", func(t *testing.T) {
		input := []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hello "},
					map[string]any{"type": "input_text", "text": "world"},
				},
			},
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hi there"},
				},
			},
		}
		msgs := responsesInputToMessages(input)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 msgs, got %d: %#v", len(msgs), msgs)
		}
		if asMap(t, msgs[0])["role"] != "user" {
			t.Fatalf("msg[0] role = %v", asMap(t, msgs[0])["role"])
		}
		if asMap(t, msgs[0])["content"] != "hello \nworld" {
			t.Fatalf("msg[0] content = %v", asMap(t, msgs[0])["content"])
		}
		if asMap(t, msgs[1])["role"] != "assistant" {
			t.Fatalf("msg[1] role = %v", asMap(t, msgs[1])["role"])
		}
	})

	t.Run("array with function_call and output", func(t *testing.T) {
		input := []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "c1",
				"name":      "getWeather",
				"arguments": `{"city":"NYC"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "c1",
				"output":  "sunny",
			},
		}
		msgs := responsesInputToMessages(input)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 msgs, got %d: %#v", len(msgs), msgs)
		}
		assistant := asMap(t, msgs[0])
		if assistant["role"] != "assistant" {
			t.Fatalf("msg[0] role = %v", assistant["role"])
		}
		tcs, ok := assistant["tool_calls"].([]any)
		if !ok || len(tcs) != 1 {
			t.Fatalf("expected one tool_call, got %#v", assistant["tool_calls"])
		}
		tc := asMap(t, asMap(t, tcs[0])["function"])
		if tc["name"] != "getWeather" || tc["arguments"] != `{"city":"NYC"}` {
			t.Fatalf("tool_call function wrong: %#v", tc)
		}
		tool := asMap(t, msgs[1])
		if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "sunny" {
			t.Fatalf("msg[1] wrong: %#v", tool)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if msgs := responsesInputToMessages(nil); msgs != nil {
			t.Fatalf("nil input should yield nil, got %#v", msgs)
		}
		if msgs := responsesInputToMessages([]any{}); len(msgs) != 0 {
			t.Fatalf("empty array should yield empty, got %#v", msgs)
		}
	})
}

// ============================================================================
// responsesToolsToChat
// ============================================================================

func TestResponsesToolsToChat(t *testing.T) {
	tools := []any{
		map[string]any{
			"type":        "function",
			"name":        "f1",
			"description": "desc",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
		map[string]any{
			// 非 function 类型应被跳过
			"type": "code_interpreter",
			"name": "ignored",
		},
	}
	out := responsesToolsToChat(tools)
	if len(out) != 1 {
		t.Fatalf("expected 1 tool after conversion, got %d: %#v", len(out), out)
	}
	o := asMap(t, out[0])
	if o["type"] != "function" {
		t.Fatalf("outer type = %v, want function", o["type"])
	}
	fn := asMap(t, o["function"])
	if fn["name"] != "f1" {
		t.Fatalf("function.name = %v, want f1", fn["name"])
	}
	if fn["description"] != "desc" {
		t.Fatalf("function.description = %v, want desc", fn["description"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("function.parameters not mapped: %#v", fn["parameters"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || props["x"] == nil {
		t.Fatalf("function.parameters.properties.x missing: %#v", params)
	}
}

// ============================================================================
// chatToResponses
// ============================================================================

func TestChatToResponsesText(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": "hello world"},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":         float64(10),
			"completion_tokens":     float64(5),
			"total_tokens":          float64(15),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(3)},
			"reasoning_tokens":      float64(2),
		},
	}
	resp := chatToResponses(chat)
	if resp["object"] != "response" {
		t.Fatalf("object = %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Fatalf("status = %v", resp["status"])
	}
	if resp["model"] != "m" {
		t.Fatalf("model = %v, want m", resp["model"])
	}
	outputs, ok := resp["output"].([]any)
	if !ok || len(outputs) != 1 {
		t.Fatalf("output len = %d", len(outputs))
	}
	msgOut := asMap(t, outputs[0])
	if msgOut["type"] != "message" || msgOut["role"] != "assistant" {
		t.Fatalf("output[0] meta wrong: %#v", msgOut)
	}
	content, ok := msgOut["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("output content len = %d", len(content))
	}
	textBlk := asMap(t, content[0])
	if textBlk["type"] != "output_text" || textBlk["text"] != "hello world" {
		t.Fatalf("output_text block wrong: %#v", textBlk)
	}
	if resp["output_text"] != "hello world" {
		t.Fatalf("output_text = %v", resp["output_text"])
	}
	usage := asMap(t, resp["usage"])
	if usage["input_tokens"] != float64(10) {
		t.Fatalf("usage.input_tokens = %v", usage["input_tokens"])
	}
	if usage["output_tokens"] != float64(5) {
		t.Fatalf("usage.output_tokens = %v", usage["output_tokens"])
	}
	if usage["total_tokens"] != float64(15) {
		t.Fatalf("usage.total_tokens = %v", usage["total_tokens"])
	}
	itd := asMap(t, usage["input_tokens_details"])
	if itd["cached_tokens"] != float64(3) {
		t.Fatalf("input_tokens_details.cached_tokens = %v", itd["cached_tokens"])
	}
	otd := asMap(t, usage["output_tokens_details"])
	if otd["reasoning_tokens"] != float64(2) {
		t.Fatalf("output_tokens_details.reasoning_tokens = %v", otd["reasoning_tokens"])
	}
}

func TestChatToResponsesToolCalls(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "f1", "arguments": `{"a":1}`},
						},
					},
				},
			},
		},
	}
	resp := chatToResponses(chat)
	outputs := resp["output"].([]any)
	// output[0] = message(text empty), output[1] = function_call
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d: %#v", len(outputs), outputs)
	}
	fc := asMap(t, outputs[1])
	if fc["type"] != "function_call" {
		t.Fatalf("output[1].type = %v, want function_call", fc["type"])
	}
	if fc["name"] != "f1" {
		t.Fatalf("function_call.name = %v, want f1", fc["name"])
	}
	if fc["arguments"] != `{"a":1}` {
		t.Fatalf("function_call.arguments = %v", fc["arguments"])
	}
}

// ============================================================================
// chatStreamToResponses (SSE 流式转换)
// ============================================================================

func TestChatStreamToResponses(t *testing.T) {
	t.Run("text delta", func(t *testing.T) {
		sse := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
			"data: [DONE]\n\n"
		resp := &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(sse)),
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		chatStreamToResponses(rec, resp, nil)
		body := rec.Body.String()
		if !strings.Contains(body, "response.output_text.delta") {
			t.Fatalf("missing text delta event\n%s", body)
		}
		if !strings.Contains(body, "Hello") {
			t.Fatalf("delta text not forwarded\n%s", body)
		}
	})
	t.Run("tool call", func(t *testing.T) {
		sse := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"f1\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n" +
			"data: [DONE]\n\n"
		resp := &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(sse)),
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		chatStreamToResponses(rec, resp, nil)
		body := rec.Body.String()
		if !strings.Contains(body, "response.output_item.added") {
			t.Fatalf("missing output_item.added event for tool call\n%s", body)
		}
		if !strings.Contains(body, "f1") {
			t.Fatalf("tool name not forwarded\n%s", body)
		}
	})
}

// TestChatStreamToResponsesEmptyGuard 空流不得静默完成: 上游只回空 delta /
// 没有任何文本/推理/工具/usage 时, 必须发 error 事件而不是 response.completed
// (2026-09-17 审查 R2-5)。
func TestChatStreamToResponsesEmptyGuard(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(sse)),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	chatStreamToResponses(rec, resp, nil)
	body := rec.Body.String()
	if strings.Contains(body, "response.completed") {
		t.Fatalf("空流不应发 response.completed\n%s", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("空流应发 error 事件\n%s", body)
	}
}

// TestChatStreamToResponsesUsageOnlyStream 只有 usage 的流是合法协议行为,
// 不应被空流防护误杀 —— **前提是 usage 里带输出侧 token**。
//
// ★ 2026-09-17 审查 P0-1 修正: 本用例原 fixture 是
// {prompt_tokens:5, completion_tokens:0, total_tokens:5} —— 纯输入侧、零输出。
// 那不是"合法 usage-only 流", 那是**真·空回包**, 断言它"应正常完成"等于把
// 缺陷锁进回归基线。现拆成两个用例: 带输出 token 的应通过, 只有输入 token 的应判空。
func TestChatStreamToResponsesUsageOnlyStream(t *testing.T) {
	sse := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(sse)),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	chatStreamToResponses(rec, resp, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("带输出侧 token 的 usage-only 流应正常完成\n%s", body)
	}
}

// TestChatStreamToResponsesInputOnlyUsageIsEmpty 只有输入侧 token 的流必须判空。
//
// 输入 token 有值只说明上游收到了 prompt, 完全不能说明它产出了东西 ——
// 这正是"成功但空"的回合, 客户端会静默结束任务。
func TestChatStreamToResponsesInputOnlyUsageIsEmpty(t *testing.T) {
	sse := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1500,\"completion_tokens\":0,\"total_tokens\":1500}}\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(sse)),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	chatStreamToResponses(rec, resp, nil)
	body := rec.Body.String()
	if strings.Contains(body, "response.completed") {
		t.Fatalf("只有输入侧 token 的流不应正常完成(应判空)\n%s", body)
	}
	if !strings.Contains(body, "empty_content") {
		t.Fatalf("只有输入侧 token 的流应发 empty_content 错误\n%s", body)
	}
}
