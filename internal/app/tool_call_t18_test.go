package app

import (
	"strings"
	"testing"
)

// 本测试对应用户要求照抄的 OmniRoute 参考实现:
//
//	open-sse/utils/stream.ts:1953              T18 finish_reason 归一
//	open-sse/handlers/responseSanitizer.ts:302 非流式 T18 归一
//	open-sse/handlers/chatCore/passthroughToolNames.ts:93
//	                                           normalizeOpenAIToolFinishReasons
//	open-sse/utils/toolCallArguments.ts:41     appendToolCallArgumentDelta
//
// 为什么重要: 这两处直接决定 agent 工具调用能否被正确识别与执行。
// 前者错了 → 工具调用被静默丢弃、任务无故中断; 后者错了 → 参数被截断或复制。

// ─────────────── T18: finish_reason 归一为 tool_calls ───────────────

// 上游在本回合用了工具调用, 却把 finish_reason 送成 "stop" —— 这正是
// OmniRoute T18 要修的场景。不修则由 agent 工具误判为"回合正常结束",
// 工具调用被丢弃, 表现为"任务无缘无故中断, 无提示无报错"。
func TestT18_流式_有工具调用但finish为stop必须归一(t *testing.T) {
	obj := map[string]any{
		"id":     "c1",
		"object": "chat.completion.chunk",
		"model":  "mimo-test",
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"id":    "call_1",
							"type":  "function",
							"function": map[string]any{
								"name":      "read_file",
								"arguments": `{"path":"a.go"}`,
							},
						},
					},
				},
				"finish_reason": "stop",
			},
		},
	}
	out := normalizeOpenAIResponse(obj)
	choices := out["choices"].([]any)
	got := choices[0].(map[string]any)["finish_reason"]
	if got != "tool_calls" {
		t.Fatalf("T18: 有 tool_calls 时 finish_reason 必须归一为 tool_calls, 实得 %v", got)
	}
}

// 无工具调用时不得乱改 finish_reason(不能把 "stop" 一律改成 "tool_calls")。
func TestT18_无工具调用时finish不变(t *testing.T) {
	obj := map[string]any{
		"id": "c1",
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": "hello"},
				"finish_reason": "stop",
			},
		},
	}
	out := normalizeOpenAIResponse(obj)
	choices := out["choices"].([]any)
	got := choices[0].(map[string]any)["finish_reason"]
	if got != "stop" {
		t.Fatalf("无工具调用时 finish_reason 必须保持 stop, 实得 %v", got)
	}
}

// finish_reason 已是 tool_calls 时保持原样(幂等)。
func TestT18_已是tool_calls保持幂等(t *testing.T) {
	obj := map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{"index": 0, "id": "call_1"}},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	out := normalizeOpenAIResponse(obj)
	choices := out["choices"].([]any)
	if got := choices[0].(map[string]any)["finish_reason"]; got != "tool_calls" {
		t.Fatalf("已是 tool_calls 应保持, 实得 %v", got)
	}
}

// 非流式形态(message.tool_calls)同样要归一 —— 对应
// responseSanitizer.ts:302 与 normalizeOpenAIToolFinishReasons。
func TestT18_非流式_有message工具调用必须归一(t *testing.T) {
	obj := map[string]any{
		"id":     "c1",
		"object": "chat.completion",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "list_dir",
								"arguments": "{}",
							},
						},
					},
				},
				"finish_reason": "stop",
			},
		},
	}
	out := normalizeOpenAIResponse(obj)
	choices := out["choices"].([]any)
	got := choices[0].(map[string]any)["finish_reason"]
	if got != "tool_calls" {
		t.Fatalf("非流式有 tool_calls 时 finish_reason 必须归一为 tool_calls, 实得 %v", got)
	}
}

// 长度为 0 的 tool_calls 数组不算"用了工具调用", 不得触发归一。
func TestT18_空工具调用数组不触发归一(t *testing.T) {
	obj := map[string]any{
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{"tool_calls": []any{}, "content": "hi"},
				"finish_reason": "stop",
			},
		},
	}
	out := normalizeOpenAIResponse(obj)
	choices := out["choices"].([]any)
	if got := choices[0].(map[string]any)["finish_reason"]; got != "stop" {
		t.Fatalf("空 tool_calls 数组不应触发归一, 实得 %v", got)
	}
}

// ───────── appendToolCallArgumentDelta: 参数分片拼接 ─────────

func TestAppendToolCallArgumentDelta(t *testing.T) {
	cases := []struct {
		name     string
		current  any
		incoming any
		want     string
		why      string
	}{
		{
			name:     "增量分片逐字拼接",
			current:  `{"path":"a`,
			incoming: `.go"}`,
			want:     `{"path":"a.go"}`,
			why:      "最常规: 两片合起来是一个完整 JSON",
		},
		{
			name:     "重复字符必须保留(ll 不能吞成 l)",
			current:  `ls -l`,
			incoming: `l`,
			want:     `ls -ll`,
			why:      "OmniRoute 明确警告: 模糊重叠去重会把 ll 吞成 l, 这是静默截断",
		},
		{
			name:     "xx 不能被吞成 x",
			current:  `-x`,
			incoming: `x`,
			want:     `-xx`,
			why:      "同上, 原注释点名的第二个反例",
		},
		{
			name:     "完全相同的快照重复 → 替换不重复",
			current:  `{"a":1}`,
			incoming: `{"a":1}`,
			want:     `{"a":1}`,
			why:      "OmniRoute #3701: 快照型上游重发全部内容, 拼接会产生重复 payload",
		},
		{
			name:     "以已有内容为前缀的增长快照 → 取后者",
			current:  `{"a":1`,
			incoming: `{"a":1,"b":2}`,
			want:     `{"a":1,"b":2}`,
			why:      "无歧义的快照增长(后者以前者开头), 取更完整的那份",
		},
		{
			name:     "非前缀的完整 JSON → 按增量追加(不猜快照)",
			current:  `{"a":1}`,
			incoming: `{"a":1,"b":2}`,
			want:     `{"a":1}{"a":1,"b":2}`,
			why: "后者并非以 `{\"a\":1}` 开头(结尾的 } 不同), 属歧义情形 —— " +
				"照抄 OmniRoute 的保守策略: 不猜, 一律按增量追加。宁可重复也不静默截断。",
		},
		{
			name:     "current 为空 → 直接取 incoming",
			current:  "",
			incoming: `{"a":1}`,
			want:     `{"a":1}`,
			why:      "首片",
		},
		{
			name:     "incoming 为空 → 保持 current",
			current:  `{"a":1}`,
			incoming: "",
			want:     `{"a":1}`,
			why:      "空分片不该影响已有内容",
		},
		{
			name:     "对象型 arguments 必须 JSON 序列化(不能丢也不能变 [object Object])",
			current:  "",
			incoming: map[string]any{"path": "a.go"},
			want:     `{"path":"a.go"}`,
			why:      "OmniRoute #6459: 部分 Anthropic 透传后端发已解析对象, 丢弃会丢参数",
		},
		{
			name:     "数组型 arguments 同理",
			current:  "",
			incoming: []any{"a", "b"},
			want:     `["a","b"]`,
			why:      "同上",
		},
		{
			name:     "nil incoming → 空字符串",
			current:  `{"a":1}`,
			incoming: nil,
			want:     `{"a":1}`,
			why:      "nil 不得破坏已有内容",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendToolCallArgumentDelta(tc.current, tc.incoming)
			if got != tc.want {
				t.Fatalf("appendToolCallArgumentDelta(%#v, %#v) = %q, 期望 %q\n理由: %s",
					tc.current, tc.incoming, got, tc.want, tc.why)
			}
		})
	}
}

// 对象型 arguments 的负向断言: 绝不能产出 Go 的 map 打印形态。
// (若用 fmt.Sprintf("%v") 而非 JSON 序列化, 就会得到 "map[path:a.go]")。
func TestAppendToolCallArgumentDelta_对象不得变成map字面量(t *testing.T) {
	got := appendToolCallArgumentDelta("", map[string]any{"path": "a.go"})
	if strings.HasPrefix(got, "map[") {
		t.Fatalf("对象型参数必须 JSON 序列化, 不得产出 Go map 字面量: %q", got)
	}
	if !strings.Contains(got, `"path"`) {
		t.Fatalf("序列化结果应含 JSON 键名, 实得 %q", got)
	}
}
