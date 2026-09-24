package translate

// 2026-09-24 审查: 协议正确性(工具调用 id / 流式 index)的回归测试。

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// firstChoiceToolCalls 从 chat completions 响应里取出 choices[0].message.tool_calls。
func firstChoiceToolCalls(t *testing.T, out map[string]any) []any {
	t.Helper()
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("响应无 choices: %+v", out)
	}
	cm, _ := choices[0].(map[string]any)
	msg, _ := cm["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	return tcs
}

// 并行调用同一个工具必须拿到**不同**的 id。
//
// 原实现用工具名占位(Gemini 原生不带 id), 于是两条 tool_call 同 id ——
// tool_result 配对错乱, compact 按 id 去重还会误删。
func TestGeminiParallelToolCallsGetDistinctIDs(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"functionCall": map[string]any{"name": "read_file", "args": map[string]any{"p": "a"}}},
					map[string]any{"functionCall": map[string]any{"name": "read_file", "args": map[string]any{"p": "b"}}},
				},
			},
			"finishReason": "STOP",
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := GeminiResponseToOpenAIChat(body)
	if err != nil {
		t.Fatalf("GeminiResponseToOpenAIChat: %v", err)
	}
	tcs := firstChoiceToolCalls(t, out)
	if len(tcs) != 2 {
		t.Fatalf("应有 2 条 tool_call, got %d", len(tcs))
	}
	id0, _ := tcs[0].(map[string]any)["id"].(string)
	id1, _ := tcs[1].(map[string]any)["id"].(string)
	if id0 == "" || id1 == "" {
		t.Fatalf("id 不得为空: %q / %q", id0, id1)
	}
	if id0 == id1 {
		t.Fatalf("并行调用同一工具必须拿到不同 id, got %q 两次", id0)
	}
}

// 上游缺 tool_use.id 时必须补一个, 不能空 id 直透(客户端会整体报错)。
func TestClaudeMissingToolUseIDGetsGenerated(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"content":     []any{map[string]any{"type": "tool_use", "name": "read_file", "input": map[string]any{}}},
		"stop_reason": "tool_use",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := ClaudeResponseToOpenAIChat(body)
	if err != nil {
		t.Fatalf("ClaudeResponseToOpenAIChat: %v", err)
	}
	tcs := firstChoiceToolCalls(t, out)
	if len(tcs) != 1 {
		t.Fatalf("应有 1 条 tool_call, got %d", len(tcs))
	}
	id, _ := tcs[0].(map[string]any)["id"].(string)
	if strings.TrimSpace(id) == "" {
		t.Fatal("缺 id 时应生成一个, 不能空 id 直透")
	}
}

// 流式 tool_calls 必须带 index —— OpenAI 协议靠它把分片拼到同一个调用上;
// 缺 index 时客户端会把每次出现当成新调用, arguments 拼成非法 JSON。
func TestGeminiSSEToolCallsCarryIndex(t *testing.T) {
	frame, err := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"role":  "model",
				"parts": []any{map[string]any{"functionCall": map[string]any{"name": "read_file", "args": map[string]any{"p": "a"}}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	src := strings.NewReader("data: " + string(frame) + "\n\n")
	var dst bytes.Buffer
	if err := GeminiSSEToOpenAISSE(src, &dst, "test-model"); err != nil {
		t.Fatalf("GeminiSSEToOpenAISSE: %v", err)
	}

	var sawToolCall bool
	for _, line := range strings.Split(dst.String(), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			delta, _ := cm["delta"].(map[string]any)
			tcs, _ := delta["tool_calls"].([]any)
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				sawToolCall = true
				if _, has := tcm["index"]; !has {
					t.Fatalf("流式 tool_call 必须带 index, got %+v", tcm)
				}
			}
		}
	}
	if !sawToolCall {
		t.Fatal("未观察到任何 tool_calls 分片, 测试前提不成立")
	}
}
