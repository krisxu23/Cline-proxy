package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicToOpenAIRequest(t *testing.T) {
	req := AnthropicRequest{
		Model:     "deepseek/deepseek-v4-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		System: json.RawMessage(`be brief`),
	}
	out := AnthropicToOpenAIRequest(req)
	msgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %#v", out)
	}
	t.Logf("got %d messages: %#v", len(msgs), msgs)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system+user), got %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Fatalf("system message: %#v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hello" {
		t.Fatalf("user message: %#v", user)
	}
}

func TestAnthropicToolUseRoundTrip(t *testing.T) {
	req := AnthropicRequest{
		Model:     "m",
		MaxTokens: 16,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read", "input": map[string]any{"path": "a.go"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
		},
	}
	out := AnthropicToOpenAIRequest(req)
	msgs := out["messages"].([]any)
	asst := msgs[0].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("assistant role: %#v", asst)
	}
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls: %#v", asst["tool_calls"])
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
		t.Fatalf("tool result: %#v", tool)
	}
}

func TestOpenAIToAnthropicResponse(t *testing.T) {
	openAI := map[string]any{
		"model": "m",
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "hi",
			},
		}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(1)},
	}
	out := OpenAIToAnthropicResponse(openAI, "m")
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason: %#v", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content: %#v", content)
	}
	block := content[0].(map[string]any)
	if block["text"] != "hi" {
		t.Fatalf("text: %#v", block)
	}
}

func TestResponsesToChatAndBack(t *testing.T) {
	body := map[string]any{
		"model":             "deepseek-v4-flash-free",
		"instructions":      "sys",
		"max_output_tokens": float64(32),
		"input":             "hello",
	}
	chat := ResponsesToChat(body)
	if chat["model"] != "deepseek-v4-flash-free" {
		t.Fatalf("model: %#v", chat["model"])
	}
	if chat["max_tokens"] != 32 {
		t.Fatalf("max_tokens: %#v", chat["max_tokens"])
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %#v", msgs)
	}
	back := ChatToResponses(map[string]any{
		"model": "deepseek-v4-flash-free",
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"},
		}},
	})
	if back["output_text"] != "ok" {
		t.Fatalf("output_text: %#v", back["output_text"])
	}
}

func TestReasonToAnthropicStop(t *testing.T) {
	if got := ReasonToAnthropicStop("length"); got != "max_tokens" {
		t.Fatalf("length -> %s", got)
	}
	if got := ReasonToAnthropicStop("tool_calls"); got != "tool_use" {
		t.Fatalf("tool_calls -> %s", got)
	}
	if got := ReasonToAnthropicStop("stop"); got != "end_turn" {
		t.Fatalf("stop -> %s", got)
	}
}

func TestAppendEmptyChunkIfNoChoice(t *testing.T) {
	var buf bytes.Buffer
	if err := AppendEmptyChunkIfNoChoice(&buf, false, "m"); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"choices"`) {
		t.Fatalf("missing choices: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Fatalf("missing DONE: %s", s)
	}
	var buf2 bytes.Buffer
	if err := AppendEmptyChunkIfNoChoice(&buf2, true, "m"); err != nil {
		t.Fatal(err)
	}
	if buf2.Len() != 0 {
		t.Fatalf("expected no write when already emitted, got %q", buf2.String())
	}
}

func TestEmptyOpenAIChunk(t *testing.T) {
	ev := EmptyOpenAIChunk("m")
	choices, _ := ev.Payload["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices: %#v", ev.Payload)
	}
}

func TestHasFinishReason(t *testing.T) {
	if HasFinishReason(map[string]any{"choices": []any{map[string]any{"finish_reason": nil}}}) {
		t.Fatal("null finish_reason must not count")
	}
	if !HasFinishReason(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop"}}}) {
		t.Fatal("stop finish_reason must count")
	}
	if HasFinishReason(map[string]any{"no": "choices"}) {
		t.Fatal("missing choices must not count")
	}
}

func TestAppendStopChunkIfNoFinish(t *testing.T) {
	var buf bytes.Buffer
	if err := AppendStopChunkIfNoFinish(&buf, false, "m"); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("missing stop finish_reason: %s", s)
	}
	if strings.Contains(s, "[DONE]") {
		t.Fatalf("stop chunk helper must not write DONE: %s", s)
	}
	var buf2 bytes.Buffer
	if err := AppendStopChunkIfNoFinish(&buf2, true, "m"); err != nil {
		t.Fatal(err)
	}
	if buf2.Len() != 0 {
		t.Fatalf("expected no write when finish seen, got %q", buf2.String())
	}
}
