package app

import (
	"strings"
	"testing"
)

// 2026-09-16 定位线上症状: muse-spark 经 zen /responses 时会偶尔返回
// "HTTP 200 + response.completed, 但可见输出为 0" 的空回合。旧实现把它当正常
// 收尾交给客户端, agent 于是静默结束任务(用户侧: 无缘无故中断, 无提示无报错)。
// 本组用例固化两条防线:
//   1. muse-spark 家族**无条件**补足输出预算(客户端不给 max_tokens 也要给);
//   2. 空回合 / 上游失败 / 流被截断都必须交付"可见的失败", 不再伪装 stop。

func TestApplyMuseSparkBudgetDefaults(t *testing.T) {
	// 客户端没给预算(Codex/cc-switch 的常态) → 必须补兜底
	out := chatBodyToResponsesBody(map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if got := anyToInt(out["max_output_tokens"]); got != museSparkDefaultOutputTokens {
		t.Fatalf("muse 家族没给预算时必须补 %d, got %d", museSparkDefaultOutputTokens, got)
	}
}

func TestApplyMuseSparkBudgetUpliftsTinyBudget(t *testing.T) {
	out := chatBodyToResponsesBody(map[string]any{
		"model":      "muse-spark-1.3-contributor-free",
		"max_tokens": 300,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if got := anyToInt(out["max_output_tokens"]); got != museSparkMinOutputTokens {
		t.Fatalf("低于下限的预算应抬到 %d, got %d", museSparkMinOutputTokens, got)
	}
	if got := zenRequestedBudget(map[string]any{"max_tokens": 300}); got != 300 {
		t.Fatalf("但 finish 修正仍应以客户端原始预算为准, got %d", got)
	}
}

func TestApplyMuseSparkBudgetKeepsExplicitBudget(t *testing.T) {
	out := chatBodyToResponsesBody(map[string]any{
		"model":      "muse-spark-1.3-contributor-free",
		"max_tokens": 4096,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if got := anyToInt(out["max_output_tokens"]); got != 4096 {
		t.Fatalf("显式预算不得被改写, got %d", got)
	}
}

func TestNonMuseModelGetsNoBudget(t *testing.T) {
	out := chatBodyToResponsesBody(map[string]any{
		"model":    "grok-code-fast-1",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, has := out["max_output_tokens"]; has {
		t.Fatal("非 muse 家族不应被注入输出预算(保持与上游默认一致)")
	}
}

// ---- 流式翻译: 空回合 / 失败 / 截断 必须可见 ----

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func TestStreamEmptyTurnReportsError(t *testing.T) {
	src := sse(
		`{"type":"response.created"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`,
	)
	var out strings.Builder
	if err := translateResponsesStreamToChat(strings.NewReader(src), &out, "muse-spark-1.3-contributor-free", 8192); err != nil {
		t.Fatalf("翻译不应返回错误(错误通过帧交付): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "upstream_empty_response") {
		t.Fatalf("空回合必须交付可见错误, got %q", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("错误帧仍需带终止位, 否则中继会再补一份终止帧: %q", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("错误帧后应补 [DONE]: %q", got)
	}
}

func TestStreamWithContentDoesNotReportError(t *testing.T) {
	src := sse(
		`{"type":"response.output_text.delta","delta":"po"}`,
		`{"type":"response.output_text.delta","delta":"ng"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}`,
	)
	var out strings.Builder
	if err := translateResponsesStreamToChat(strings.NewReader(src), &out, "muse-spark-1.3-contributor-free", 8192); err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	got := out.String()
	if strings.Contains(got, `"error"`) {
		t.Fatalf("有正文时不得注入 error 帧: %q", got)
	}
	if !strings.Contains(got, `"content":"po"`) || !strings.Contains(got, `"content":"ng"`) {
		t.Fatalf("正文必须原样交付: %q", got)
	}
}

func TestStreamToolCallCountsAsDelivered(t *testing.T) {
	// 只回工具调用、没有正文 —— 这是有效交付, 不能误判成空回合。
	src := sse(
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":\"a\"}"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`,
	)
	var out strings.Builder
	if err := translateResponsesStreamToChat(strings.NewReader(src), &out, "muse-spark-1.3-contributor-free", 8192); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	got := out.String()
	if strings.Contains(got, `"error"`) {
		t.Fatalf("有工具调用时不得判为空回合: %q", got)
	}
	if !strings.Contains(got, `"name":"read_file"`) {
		t.Fatalf("工具调用必须交付: %q", got)
	}
}

func TestStreamFailedEventSurfacesReason(t *testing.T) {
	src := sse(
		`{"type":"response.output_text.delta","delta":"部分"}`,
		`{"type":"response.failed","response":{"error":{"message":"worker crashed"}}}`,
	)
	var out strings.Builder
	if err := translateResponsesStreamToChat(strings.NewReader(src), &out, "muse-spark-1.3-contributor-free", 8192); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "upstream_stream_failed") || !strings.Contains(got, "worker crashed") {
		t.Fatalf("上游失败必须带原因交付: %q", got)
	}
	if !strings.Contains(got, "部分") {
		t.Fatalf("已交付的部分正文不能丢: %q", got)
	}
}

func TestStreamTruncatedWithoutCompletionReportsError(t *testing.T) {
	// 上游把连接掐了: 没有 completed/incomplete 事件
	src := sse(`{"type":"response.output_text.delta","delta":"半截"}`)
	var out strings.Builder
	if err := translateResponsesStreamToChat(strings.NewReader(src), &out, "muse-spark-1.3-contributor-free", 8192); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "upstream_stream_truncated") {
		t.Fatalf("流被截断必须可见: %q", got)
	}
	if !strings.Contains(got, "半截") {
		t.Fatalf("已交付内容必须保留: %q", got)
	}
}

func TestResponsesChatEmptyDetection(t *testing.T) {
	empty := map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}}}}
	if !responsesChatEmpty(empty) {
		t.Fatal("无正文无工具调用应判为空")
	}
	withText := map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"content": "hi"}}}}
	if responsesChatEmpty(withText) {
		t.Fatal("有正文不应判为空")
	}
	withTools := map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"content": "", "tool_calls": []any{map[string]any{"id": "c1"}}}}}}
	if responsesChatEmpty(withTools) {
		t.Fatal("有工具调用不应判为空")
	}
	if !responsesChatEmpty(map[string]any{}) {
		t.Fatal("没有 choices 应判为空")
	}
}
