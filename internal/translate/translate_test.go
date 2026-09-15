package translate

// OpenAI → Anthropic 请求翻译的协议夹具测试(覆盖真实客户端请求的主要形态)。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIChatToClaudeRequestCore(t *testing.T) {
	body := map[string]any{
		"model":       "claude-sonnet-4",
		"max_tokens":  float64(4096),
		"temperature": float64(0.7),
		"stop":        "STOP",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "看这张图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,AAAA",
				}},
			}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "get_weather", "arguments": `{"city":"北京"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴天 25 度"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
		"tool_choice": "auto",
	}

	out, err := OpenAIChatToClaudeRequest("claude-sonnet-4", body, true)
	if err != nil {
		t.Fatal(err)
	}

	// 必填与采样
	if out["model"] != "claude-sonnet-4" || out["max_tokens"] != 4096 || out["stream"] != true {
		t.Fatalf("model/max_tokens/stream 不符: %v", out)
	}
	if out["temperature"] != float64(0.7) {
		t.Fatalf("temperature 应保留: %v", out["temperature"])
	}
	if ss, ok := out["stop_sequences"].([]any); !ok || len(ss) != 1 || ss[0] != "STOP" {
		t.Fatalf("stop → stop_sequences 映射错误: %v", out["stop_sequences"])
	}

	// system 提取到顶层
	sys, ok := out["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system 应提取为顶层块: %v", out["system"])
	}

	// 消息序列: user(文本+图) / assistant(tool_use) / user(tool_result)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应有 3 条消息, got %d", len(msgs))
	}
	u0 := msgs[0].(map[string]any)
	if u0["role"] != "user" {
		t.Fatalf("首条应为 user, got %v", u0["role"])
	}
	b0, _ := u0["content"].([]any)
	if len(b0) != 2 || b0[1].(map[string]any)["type"] != "image" {
		t.Fatalf("user 消息应含图片块: %v", b0)
	}
	src := b0[1].(map[string]any)["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Fatalf("data URL 应解析为 base64 source: %v", src)
	}

	a1 := msgs[1].(map[string]any)
	if a1["role"] != "assistant" {
		t.Fatal("第二条应为 assistant")
	}
	a1b, _ := a1["content"].([]any)
	tu := a1b[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" {
		t.Fatalf("tool_calls 应映射为 tool_use: %v", tu)
	}
	input := tu["input"].(map[string]any)
	if input["city"] != "北京" {
		t.Fatalf("arguments 应解析为对象: %v", input)
	}

	u2 := msgs[2].(map[string]any)
	b2, _ := u2["content"].([]any)
	tr := b2[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Fatalf("tool 消息应映射为 tool_result: %v", tr)
	}

	// tools 与 tool_choice
	cts, _ := out["tools"].([]any)
	ct := cts[0].(map[string]any)
	if ct["name"] != "get_weather" || ct["input_schema"] == nil {
		t.Fatalf("function.parameters 应映射为 input_schema: %v", ct)
	}
	if tc, ok := out["tool_choice"].(map[string]any); !ok || tc["type"] != "auto" {
		t.Fatalf("tool_choice=auto 应映射: %v", out["tool_choice"])
	}
}

func TestOpenAIChatToClaudeRequestThinkingAndEdge(t *testing.T) {
	// reasoning_effort → thinking(enabled+budget), 且 temperature 被移除
	body := map[string]any{
		"model":            "claude-sonnet-4",
		"max_tokens":       float64(8192),
		"temperature":      float64(0.5),
		"reasoning_effort": "high",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := OpenAIChatToClaudeRequest("claude-sonnet-4", body, false)
	if err != nil {
		t.Fatal(err)
	}
	th, ok := out["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" || th["budget_tokens"] != 131072 {
		t.Fatalf("reasoning_effort=high 应映射为 thinking(enabled,131072): %v", out["thinking"])
	}
	if _, has := out["temperature"]; has {
		t.Fatal("thinking 激活时必须移除 temperature")
	}

	// 无 max_tokens: 8192 兜底(Anthropic 必填)
	out2, err := OpenAIChatToClaudeRequest("m", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out2["max_tokens"] != 8192 {
		t.Fatalf("缺省 max_tokens 应为 8192, got %v", out2["max_tokens"])
	}

	// registry 取用
	if GetRequestTranslator(FormatOpenAI, FormatClaude) == nil {
		t.Fatal("openai:claude 请求翻译器应已注册")
	}
	if GetRequestTranslator(FormatClaude, FormatOpenAI) != nil {
		t.Fatal("尚未注册的反向翻译器应为 nil")
	}
	// JSON 往返: 翻译产物必须是合法 JSON
	b, err := json.Marshal(out2)
	if err != nil || !strings.HasPrefix(string(b), "{") {
		t.Fatalf("产物应可序列化: %v", err)
	}
}
