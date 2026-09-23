package translate

// Anthropic → OpenAI 响应方向翻译的测试(非流式 body + SSE 事件流)。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeResponseToOpenAIChat(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude-x","stop_reason":"tool_use",
		"content":[{"type":"text","text":"查一下"},
			{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"北京"}}],
		"usage":{"input_tokens":10,"output_tokens":5}}`)
	out, err := ClaudeResponseToOpenAIChat(body)
	if err != nil {
		t.Fatal(err)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "查一下" {
		t.Fatalf("content 不符: %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "tu_1" {
		t.Fatalf("tool_call id 不符: %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name 不符: %v", fn)
	}
	var args map[string]any
	json.Unmarshal([]byte(fn["arguments"].(string)), &args)
	if args["city"] != "北京" {
		t.Fatalf("arguments 应为 JSON 对象: %v", fn["arguments"])
	}
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("tool_use 的 stop_reason 应映射为 tool_calls, got %v", ch["finish_reason"])
	}
	u := out["usage"].(map[string]any)
	// JSON 解码后数值为 float64, 按数值比较
	if int(u["prompt_tokens"].(float64)) != 10 || int(u["completion_tokens"].(float64)) != 5 {
		t.Fatalf("usage 映射不符: %v", u)
	}
}

func TestClaudeSSEToOpenAISSE(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_9\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var sb strings.Builder
	if err := ClaudeSSEToOpenAISSE(strings.NewReader(sse), &sb, "claude-x"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`"role":"assistant"`, `你好`, `"reasoning_content":"think"`,
		`"id":"tu_9"`, `"name":"f"`, `"arguments":"{\"a\":1}"`,
		`"finish_reason":"tool_calls"`, "data: [DONE]",
		// 末帧 usage 必须合并 message_start 的 input_tokens(9)+message_delta 的
		// output_tokens(7) → prompt+completion+total 齐全, 不能覆盖成 null/total=7。
		`"usage":{"completion_tokens":7,"prompt_tokens":9,"total_tokens":16}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE 缺少 %q:\n%s", want, out)
		}
	}
	// 无终止事件时: 上游没发 message_delta → 应合成 finish(另案验证)
	noStop := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"
	var sb2 strings.Builder
	if err := ClaudeSSEToOpenAISSE(strings.NewReader(noStop), &sb2, "m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb2.String(), `"finish_reason":"stop"`) {
		t.Fatalf("无终止事件应合成 finish:\n%s", sb2.String())
	}
}
