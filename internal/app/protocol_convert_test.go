package app

import (
	"encoding/json"
	"testing"
)

// ============================================================================
// anthropicToOpenAI
// ============================================================================

func TestAnthropicToOpenAI(t *testing.T) {
	// 隔离工作目录: loadOverrideContent 会从 CWD 读取可选的 override.md(系统提示词覆盖)。
	// 若并发环境里别的进程在包目录下残留了 override.md, 会遮蔽传入的 System 字段,
	// 导致本测试对「System 透传」的断言失真。切到空临时目录保证可复现。
	t.Chdir(t.TempDir())
	req := anthropicReq{
		Model:       "claude-3-5-sonnet",
		MaxTokens:   1024,
		Stream:      true,
		Temperature: 0.5,
		System:      json.RawMessage(`"system prompt"`),
		Tools: json.RawMessage(`[
			{"name":"f1","description":"desc","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}
		]`),
		ToolChoice: json.RawMessage(`{"type":"auto"}`),
		Messages: []anthropicMsg{
			{Role: "user", Content: []any{map[string]any{"type": "text", "text": "use tool"}}},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "ok"},
				map[string]any{"type": "tool_use", "id": "tu1", "name": "f1", "input": map[string]any{"a": "x"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "42"},
			}},
		},
	}

	out := anthropicToOpenAI(req)

	// model 透传
	if out["model"] != "claude-3-5-sonnet" {
		t.Fatalf("model passthrough: got %v", out["model"])
	}
	// max_tokens
	if out["max_tokens"] != 1024 {
		t.Fatalf("max_tokens: got %v", out["max_tokens"])
	}
	// stream / temperature
	if out["stream"] != true {
		t.Fatalf("stream: got %v", out["stream"])
	}
	if out["temperature"] != 0.5 {
		t.Fatalf("temperature: got %v", out["temperature"])
	}

	// system -> messages[0]
	// 输入是 system + user + assistant(tool_use) + tool_result 四段, 正确输出就是 4 条:
	// system / user / assistant(带 tool_calls) / tool(带 tool_call_id)。
	// 下方第 msgs[3] 的断言本身就要求有第 4 条, 所以这里必须是 4, 原来的 3 是笔误。
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(msgs), msgs)
	}
	sys := asMap(t, msgs[0])
	if sys["role"] != "system" || sys["content"] != "system prompt" {
		t.Fatalf("system message wrong: %#v", sys)
	}

	// user text
	user0 := asMap(t, msgs[1])
	if user0["role"] != "user" || user0["content"] != "use tool" {
		t.Fatalf("user message wrong: %#v", user0)
	}

	// assistant with tool_use -> tool_calls
	asst := asMap(t, msgs[2])
	if asst["role"] != "assistant" {
		t.Fatalf("assistant role wrong: %#v", asst["role"])
	}
	tcs := asMap(t, asst)["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tcFn := asMap(t, asMap(t, tcs[0])["function"])
	if tcFn["name"] != "f1" {
		t.Fatalf("tool_call name = %v", tcFn["name"])
	}
	if tcFn["arguments"] != `{"a":"x"}` {
		t.Fatalf("tool_call arguments = %v", tcFn["arguments"])
	}

	// tool_result -> separate tool message
	toolMsg := asMap(t, msgs[3])
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool_result message role = %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "tu1" || toolMsg["content"] != "42" {
		t.Fatalf("tool_result message wrong: %#v", toolMsg)
	}

	// tools: input_schema -> function.parameters
	tools := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	fn := asMap(t, asMap(t, tools[0])["function"])
	if fn["name"] != "f1" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	params := asMap(t, fn["parameters"])
	if params["type"] != "object" {
		t.Fatalf("function.parameters.type = %v", params["type"])
	}
	props := asMap(t, params["properties"])
	if _, ok := props["a"]; !ok {
		t.Fatalf("function.parameters.properties.a missing: %#v", params)
	}

	// tool_choice 透传
	if !equalAny(out["tool_choice"], map[string]any{"type": "auto"}) {
		t.Fatalf("tool_choice wrong: %#v", out["tool_choice"])
	}
}

func TestAnthropicToolsToOpenAI_PassthroughForm(t *testing.T) {
	// 已经是 OpenAI 格式的 tool(type=function) 应原样透传
	tools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "f1", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "x", "input_schema": map[string]any{"type": "object"}},
	}
	out := anthropicToolsToOpenAI(tools)
	if len(out) != 2 {
		t.Fatalf("expected 2 tools passthrough, got %d", len(out))
	}
}

// ============================================================================
// openAIToAnthropic
// ============================================================================

func TestOpenAIToAnthropic(t *testing.T) {
	openAI := map[string]any{
		"model": "claude-3-5-sonnet",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello there",
					"tool_calls": []any{
						map[string]any{
							"id":       "call1",
							"type":     "function",
							"function": map[string]any{"name": "f1", "arguments": `{"a":1}`},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     float64(10),
			"completion_tokens": float64(3),
			"total_tokens":      float64(13),
		},
	}

	out := openAIToAnthropic(openAI)
	if out["role"] != "assistant" {
		t.Fatalf("role = %v", out["role"])
	}
	if out["model"] != "claude-3-5-sonnet" {
		t.Fatalf("model = %v", out["model"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", out["stop_reason"])
	}
	content := out["content"].([]any)
	// text block + tool_use block
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d: %#v", len(content), content)
	}
	if asMap(t, content[0])["type"] != "text" || asMap(t, content[0])["text"] != "hello there" {
		t.Fatalf("text block wrong: %#v", content[0])
	}
	tu := asMap(t, content[1])
	if tu["type"] != "tool_use" {
		t.Fatalf("block[1].type = %v, want tool_use", tu["type"])
	}
	if tu["name"] != "f1" || tu["id"] != "call1" {
		t.Fatalf("tool_use meta wrong: %#v", tu)
	}
	input := asMap(t, tu["input"])
	if input["a"].(float64) != 1 {
		t.Fatalf("tool_use input.a = %v", input["a"])
	}
	usage := asMap(t, out["usage"])
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage mapping wrong: %#v", usage)
	}
}

func TestOpenAIToAnthropicTextOnly(t *testing.T) {
	openAI := map[string]any{
		"choices": []any{
			map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "plain"},
				"finish_reason": "stop",
			},
		},
	}
	out := openAIToAnthropic(openAI)
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", out["stop_reason"])
	}
	content := out["content"].([]any)
	if asMap(t, content[0])["text"] != "plain" {
		t.Fatalf("content text wrong: %#v", content[0])
	}
}

func TestRoundTripAnthropicOpenAI(t *testing.T) {
	// anthropicToOpenAI -> (wrap as openai response) -> openAIToAnthropic
	// 验证 role / content(text) / tool_calls 不丢失
	req := anthropicReq{
		Model: "m",
		Messages: []anthropicMsg{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "thinking"},
				map[string]any{"type": "tool_use", "id": "tu1", "name": "f1", "input": map[string]any{"x": 1}},
			}},
		},
	}
	openAIReq := anthropicToOpenAI(req)
	msgs := openAIReq["messages"].([]any)
	var asst map[string]any
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
			asst = mm
		}
	}
	if asst == nil {
		t.Fatalf("assistant message not found in converted request")
	}
	openAIResp := map[string]any{
		"model": openAIReq["model"],
		"choices": []any{
			map[string]any{"message": asst, "finish_reason": "tool_calls"},
		},
	}
	anth := openAIToAnthropic(openAIResp)
	if anth["role"] != "assistant" {
		t.Fatalf("role lost: %v", anth["role"])
	}
	content := anth["content"].([]any)
	foundText, foundTool := false, false
	for _, c := range content {
		cm := asMap(t, c)
		switch cm["type"] {
		case "text":
			if cm["text"] == "thinking" {
				foundText = true
			}
		case "tool_use":
			foundTool = true
			if cm["name"] != "f1" {
				t.Fatalf("tool name lost: %v", cm["name"])
			}
			if asMap(t, cm["input"])["x"].(float64) != 1 {
				t.Fatalf("tool input lost: %#v", cm["input"])
			}
		}
	}
	if !foundText {
		t.Fatalf("text content lost in roundtrip")
	}
	if !foundTool {
		t.Fatalf("tool_use content lost in roundtrip")
	}
}

// ============================================================================
// filterToolInput
// ============================================================================

func TestFilterToolInput(t *testing.T) {
	schemas := map[string]map[string]bool{
		"f1": {"a": true, "b": true},
	}

	t.Run("filter extra keys", func(t *testing.T) {
		out := filterToolInput("f1", map[string]any{"a": 1, "b": 2, "c": 3}, schemas)
		if len(out) != 2 || out["a"] != 1 || out["b"] != 2 {
			t.Fatalf("expected {a:1,b:2}, got %#v", out)
		}
		if _, ok := out["c"]; ok {
			t.Fatalf("field c should be filtered out: %#v", out)
		}
	})

	t.Run("unknown tool keeps all", func(t *testing.T) {
		in := map[string]any{"a": 1, "c": 3}
		out := filterToolInput("unknown", in, schemas)
		if !equalAny(out, in) {
			t.Fatalf("unknown tool should pass input through, got %#v", out)
		}
	})

	t.Run("filtered empty falls back to input", func(t *testing.T) {
		in := map[string]any{"c": 3}
		out := filterToolInput("f1", in, schemas)
		if !equalAny(out, in) {
			t.Fatalf("empty filter result should fall back to input, got %#v", out)
		}
	})

	t.Run("empty allowed set keeps input", func(t *testing.T) {
		in := map[string]any{"a": 1}
		out := filterToolInput("f1", in, map[string]map[string]bool{"f1": {}})
		if !equalAny(out, in) {
			t.Fatalf("empty allowed set should keep input, got %#v", out)
		}
	})
}

// ============================================================================
// extractToolSchemas
// ============================================================================

func TestExtractToolSchemas(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if out := extractToolSchemas(nil); len(out) != 0 {
			t.Fatalf("nil tools should yield empty map, got %#v", out)
		}
		if out := extractToolSchemas(json.RawMessage(`[]`)); len(out) != 0 {
			t.Fatalf("empty tools should yield empty map, got %#v", out)
		}
	})

	t.Run("valid schemas", func(t *testing.T) {
		raw := json.RawMessage(`[
			{"name":"f1","input_schema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]}},
			{"name":"f2","input_schema":{"type":"object"}}
		]`)
		out := extractToolSchemas(raw)
		// f2 没有 properties, 按实现会被跳过 (proxy.go:1753 if len(props)==0 { continue })
		if len(out) != 1 {
			t.Fatalf("expected 1 schema (f2 skipped for no properties), got %d: %#v", len(out), out)
		}
		allowed, ok := out["f1"]
		if !ok {
			t.Fatalf("f1 missing from schemas: %#v", out)
		}
		if !allowed["a"] || !allowed["b"] {
			t.Fatalf("f1 allowed props wrong: %#v", allowed)
		}
		if _, ok := out["f2"]; ok {
			t.Fatalf("f2 (no properties) should be skipped, got %#v", out)
		}
	})

	t.Run("no properties skipped", func(t *testing.T) {
		raw := json.RawMessage(`[{"name":"f1","input_schema":{"type":"object"}}]`)
		out := extractToolSchemas(raw)
		if _, ok := out["f1"]; ok {
			t.Fatalf("tool without properties should be skipped, got %#v", out)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		out := extractToolSchemas(json.RawMessage(`not json`))
		if len(out) != 0 {
			t.Fatalf("invalid json should yield empty map, got %#v", out)
		}
	})
}
