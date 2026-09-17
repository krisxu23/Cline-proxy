package app

// anthropic 流式路径的空流守卫测试(2026-09-17 审查 P0-2)。
//
// 背景: 三条流式入口里, chat 与 responses 都有空流守卫, 唯独本路径没有 ——
// 上游 200 空手而归时, Claude 协议客户端会拿到一个干净的 end_turn:
// 不报错、不重试, 任务静默中断。
//
// 本文件锁定两件相反的事:
//   1. 零产出的回合必须发 error 而非 message_stop(不静默成功);
//   2. 合法回合(有正文 / 合法空终止态 / 纯工具调用)照常走完, 不得误杀。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropicGuardDrain 驱动真实的 anthropic 流式回写函数, 返回客户端看到的全部字节。
func anthropicGuardDrain(t *testing.T, upstreamBody string) string {
	t.Helper()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	handleAnthropicStreamWithToolNameMap(rec, resp, "mimo-test", nil, nil, nil)
	return rec.Body.String()
}

func dataFrame(payload string) string { return "data: " + payload + "\n\n" }

// TestAnthropicStream_零产出必须发error 上游只回脚手架帧时必须发 error, 不得静默 end_turn。
func TestAnthropicStream_零产出必须发error(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"只有 role 骨架帧",
			dataFrame(`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			"只有 finish_reason=stop",
			dataFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			"只有空白 reasoning",
			dataFrame(`{"choices":[{"index":0,"delta":{"reasoning_content":"   "}}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			"什么都没有(立即 EOF)",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := anthropicGuardDrain(t, c.body)
			if strings.Contains(out, "message_stop") {
				t.Fatalf("零产出回合不得发 message_stop(会变成静默 end_turn), 实得:\n%s", out)
			}
			if !strings.Contains(out, "event: error") {
				t.Fatalf("零产出回合应发 error 事件, 实得:\n%s", out)
			}
			if !strings.Contains(out, "empty_content") {
				t.Fatalf("error 事件应带 empty_content 类型, 实得:\n%s", out)
			}
		})
	}
}

// TestAnthropicStream_合法回合不得误杀 有产出的回合必须照常收尾。
func TestAnthropicStream_合法回合不得误杀(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"有正文",
			dataFrame(`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`) +
				dataFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			"有非空白推理",
			dataFrame(`{"choices":[{"index":0,"delta":{"reasoning_content":"想一下"}}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			"纯工具调用",
			dataFrame(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]}}]}`) +
				dataFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
				dataFrame(`[DONE]`),
		},
		{
			// 被 token 上限截断: 本就不该有正文, 是合法成功。
			"合法空终止态 length",
			dataFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`) +
				dataFrame(`[DONE]`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := anthropicGuardDrain(t, c.body)
			if strings.Contains(out, "empty_content") {
				t.Fatalf("%s 是合法回合, 不得判为空流, 实得:\n%s", c.name, out)
			}
			if !strings.Contains(out, "message_stop") {
				t.Fatalf("%s 应正常收尾(message_stop), 实得:\n%s", c.name, out)
			}
		})
	}
}

// TestAnthropicStream_无名工具块不算产出 工具块要按"能不能发出"计数 ——
// emitToolBlock 会跳过无名的块, 所以 pendingTools 非空不等于有产出。
func TestAnthropicStream_无名工具块不算产出(t *testing.T) {
	// 只有 arguments 没有 function.name → 块发不出去 → 仍是零产出
	out := anthropicGuardDrain(t,
		dataFrame(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`)+
			dataFrame(`[DONE]`))

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("无名工具块发不出去, 应判为零产出, 实得:\n%s", out)
	}
	if strings.Contains(out, `"name":"read_file"`) {
		t.Fatalf("不应发出任何工具块, 实得:\n%s", out)
	}
}
