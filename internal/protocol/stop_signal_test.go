package protocol

// 跨协议终止判定的测试(参照 OmniRoute checkIfStopSignal 的语义对齐)。

import "testing"

func TestHasStopSignalAcrossProtocols(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"OpenAI finish_reason", map[string]any{
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop"}},
		}, true},
		{"OpenAI 无 finish_reason", map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "x"}}},
		}, false},
		{"Gemini candidates.finishReason", map[string]any{
			"candidates": []any{map[string]any{"finishReason": "STOP"}},
		}, true},
		{"Anthropic content_block_stop", map[string]any{"type": "content_block_stop"}, true},
		{"Anthropic message_stop", map[string]any{"type": "message_stop"}, true},
		{"Anthropic message_delta.stop_reason", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
		}, true},
		{"Anthropic message_delta 无 stop_reason", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"text": "x"},
		}, false},
		{"Responses response.completed", map[string]any{"type": "response.completed"}, true},
		{"Responses response.failed", map[string]any{"type": "response.failed"}, true},
		{"Responses 增量事件", map[string]any{"type": "response.output_text.delta", "delta": "x"}, false},
		{"空对象", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := HasStopSignal(c.obj); got != c.want {
			t.Fatalf("%s: HasStopSignal=%v, want %v", c.name, got, c.want)
		}
	}

	// 与旧行为兼容: OpenAI 形态下 HasStopSignal 必须包含 HasFinishReason 的全部命中
	oa := map[string]any{"choices": []any{map[string]any{"finish_reason": "length"}}}
	if !HasFinishReason(oa) || !HasStopSignal(oa) {
		t.Fatal("OpenAI 终止形态两者都应命中")
	}
}
