package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本测试对应用户要求照抄的 OmniRoute 参考用例:
//
//	open-sse/utils/streamEmptyChoices.ts       → rejectEmptyChoicesStream(空流拒绝)
//	tests/unit/stream-empty-choices-interceptor.test.ts → 三个场景:
//	  1. 全空 choices 流必须失败, 不得以干净 200 + [DONE] 收尾(#9268)
//	  2. 有真实内容的流正常通过
//	  3. 真实内容之后的空 choices 帧不影响通过
//
// 与参考实现的差异(该差异是必要的适配, 不是自造逻辑): OmniRoute 用
// controller.error 表达 502; 本网关的流式 200 已提交, HTTP 状态码改不了,
// 因此失败信息走 SSE 帧(error.type = "empty_content" + finish_reason)。

// ─────────────────── openAIChunkHasValuableContent 判定 ───────────────────

func TestOpenAIChunkHasValuableContent(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{
			name: "delta 带非空 content → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}},
			}},
			want: true,
		},
		{
			name: "delta 带 tool_calls → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{
					"tool_calls": []any{map[string]any{"id": "call_1"}},
				}},
			}},
			want: true,
		},
		{
			name: "带 finish_reason → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			want: true,
		},
		{
			name: "非流式 message 形态 → 无价值(hasValuableContent 只认 delta)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "message": map[string]any{"content": "hi"}},
			}},
			want: false,
		},
		{
			name: "只有 role 骨架帧 → 有价值(hasValuableContent 显式接受 role)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}},
			}},
			want: true,
		},
		{
			name: "delta 带 reasoning_content → 有价值(hasAnyReasoningSignal)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "想一下"}},
			}},
			want: true,
		},
		{
			name: "多 choice 时只看 choices[0] → 第一个空即无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": ""}},
				map[string]any{"index": 1, "delta": map[string]any{"content": "有内容"}},
			}},
			want: false,
		},
		// ── 以下均对应参考用例里的 "emptyChoicesChunk" / 骨架帧 ──
		{
			name: "空 choices 数组 → 无价值(#9268 核心场景)",
			obj:  map[string]any{"id": "chatcmpl-1", "model": "m", "choices": []any{}},
			want: false,
		},
		{
			name: "choices 缺失 → 无价值",
			obj:  map[string]any{"id": "chatcmpl-1", "model": "m"},
			want: false,
		},
		{
			name: "content 为空字符串 → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": ""}},
			}},
			want: false,
		},
		{
			name: "finish_reason 为空字符串 → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": ""},
			}},
			want: false,
		},
		{
			name: "tool_calls 为空数组 → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{}}},
			}},
			want: false,
		},
		{
			name: "choices 元素类型异常 → 不 panic 且无价值",
			obj:  map[string]any{"choices": []any{"not an object", 42}},
			want: false,
		},
		{
			name: "delta 缺失(choices[0] 只有 finish_reason) → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "finish_reason": "stop"},
			}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIChunkHasValuableContent(tc.obj); got != tc.want {
				t.Fatalf("openAIChunkHasValuableContent = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// ─────────────────── 端到端: 真实 handleStreamResponseWithUsage ───────────────────

// drainStream 用 httptest 驱动真实的流式处理函数, 返回客户端看到的全部字节。
// 上游 body 由 frames 顺序拼接而成。
func drainStream(t *testing.T, upstreamBody string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}
	upstream.Body = nopCloser{strings.NewReader(upstreamBody)}
	handleStreamResponseWithUsage(rec, upstream, nil)
	return rec.Body.String()
}

// 参考用例里的 emptyChoicesChunk。
func emptyChoicesFrame(id string) string {
	return "data: " + `{"id":"chatcmpl-` + id + `","object":"chat.completion.chunk","model":"mimo-test","choices":[]}` + "\n\n"
}

func contentFrame(text string) string {
	return "data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n"
}

func finishFrame() string {
	return "data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
}

// 对应用例 1: "an all-empty-choices stream is rejected as a retryable error"。
// 参考断言: errored || !output.includes("[DONE]")。
// 本网关的等价契约: 绝不能出现"干净的空 200"—— 必须带 error 帧。
func Test空流_全空choices必须失败(t *testing.T) {
	out := drainStream(t, emptyChoicesFrame("1")+emptyChoicesFrame("2"))

	if !strings.Contains(out, `"empty_content"`) {
		t.Fatalf("全空 choices 流必须交付 empty_content 错误帧, 实得:\n%s", out)
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("必须带 error 对象, 实得:\n%s", out)
	}
	// 参考断言 `!output.includes("[DONE]")` 在本网关的对应形式:
	// 允许 [DONE](客户端需要终止信号), 但它必须与 error 帧同时出现,
	// 绝不能是"只有 [DONE] 的干净收尾"。
	if strings.Contains(out, "[DONE]") && !strings.Contains(out, "empty_content") {
		t.Fatalf("只有 [DONE] 的干净空 200 正是要根治的静默中断, 实得:\n%s", out)
	}
}

// 对应用例 2: "a stream with real content passes through unchanged"。
func Test空流_有真实内容正常通过(t *testing.T) {
	out := drainStream(t, contentFrame("hello")+finishFrame())

	if !strings.Contains(out, "hello") {
		t.Fatalf("真实内容必须被转发, 实得:\n%s", out)
	}
	if strings.Contains(out, "empty_content") {
		t.Fatalf("健康流不得被判为空流, 实得:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("正常收尾应带 [DONE], 实得:\n%s", out)
	}
}

// 对应用例 3: "empty choices after real content still passes through"。
func Test空流_真实内容之后的空choices不影响通过(t *testing.T) {
	out := drainStream(t, contentFrame("real output")+emptyChoicesFrame("1")+finishFrame())

	if !strings.Contains(out, "real output") {
		t.Fatalf("真实内容必须被转发, 实得:\n%s", out)
	}
	if strings.Contains(out, "empty_content") {
		t.Fatalf("先有内容后有空帧不得被判为空流, 实得:\n%s", out)
	}
}

// role 骨架帧本身**算**有价值内容(hasValuableContent 显式接受 role)——
// 这条与直觉相反, 但必须照抄。因此"只有骨架帧 + [DONE]"的流会正常收尾。
// 真正会被判空的是"一件事都没做"的情形, 见 Test空流_只有usage没有choices必须失败。
func Test空流_仅骨架帧按参考实现算有价值(t *testing.T) {
	skeleton := "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	out := drainStream(t, skeleton+"data: [DONE]\n\n")

	if strings.Contains(out, "empty_content") {
		t.Fatalf("骨架帧按 hasValuableContent 应算有价值, 不应判空流, 实得:\n%s", out)
	}
}

// usage-only 流是**合法**的(OmniRoute: "usage-only streams are fine")。
// 上游报告了真实 token 用量, 说明这一回合确实发生过, 不该判成静默中断。
func Test空流_仅真实usage算有效交付(t *testing.T) {
	out := drainStream(t, "data: "+`{"id":"c1","model":"m","usage":{"prompt_tokens":1},"choices":[]}`+"\n\n")

	if strings.Contains(out, "empty_content") {
		t.Fatalf("带真实 usage 的流不应判空(usage-only 合法), 实得:\n%s", out)
	}
}

// 全零 usage 是空壳, 不算有效用量(对应 hasValidUsage 的 `> 0` 判据)。
func Test空流_全零usage不算有效(t *testing.T) {
	out := drainStream(t, "data: "+`{"id":"c1","model":"m","usage":{"prompt_tokens":0,"completion_tokens":0},"choices":[]}`+"\n\n")

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("全零 usage 应判为空流, 实得:\n%s", out)
	}
}

// 关键回归: 上游一帧内容都不给、直接发 [DONE]。
// 网关会为缺少 finish_reason 的上游**自己合成**一个终止帧 —— 那个合成帧是
// 脚手架, 绝不能被算成"上游交付了有价值内容", 否则这条最典型的静默中断
// (上游空手而归 + [DONE]) 会绕过空流防护, 退回成干净的空 200。
func Test空流_只发DONE不合成价值(t *testing.T) {
	out := drainStream(t, "data: [DONE]\n\n")

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("上游只发 [DONE] 必须判空流, 不得因网关自补的终止帧而放行, 实得:\n%s", out)
	}
}

// 同上, 但上游先发若干空 choices 帧再发 [DONE]。
func Test空流_空choices后接DONE必须失败(t *testing.T) {
	out := drainStream(t, emptyChoicesFrame("1")+"data: [DONE]\n\n")

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("空 choices + [DONE] 必须判空流, 实得:\n%s", out)
	}
}

// 合成路径 1: 完整 JSON body 回空壳 choices —— 必须同样被判空流。
// (这是自查发现的缺陷: 合成路径早期绕过了空流判定。)
func Test空流_完整JSON空壳body必须失败(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
	out := drainStream(t, body)

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("完整 JSON 空壳 body 应判为空流, 实得:\n%s", out)
	}
}

// 合成路径 2: 完整 JSON body 带真实内容 —— 必须正常通过(负向对照)。
func Test空流_完整JSON有内容正常通过(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"real answer"},"finish_reason":"stop"}]}`
	out := drainStream(t, body)

	if !strings.Contains(out, "real answer") {
		t.Fatalf("合成路径的真实内容必须被转发, 实得:\n%s", out)
	}
	if strings.Contains(out, "empty_content") {
		t.Fatalf("有内容的完整 JSON 不得判为空流, 实得:\n%s", out)
	}
}

// 合成路径 3: NDJSON 逐行空 choices —— 必须被判空流。
func Test空流_NDJSON空choices必须失败(t *testing.T) {
	line := `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[]}`
	out := drainStream(t, line+"\n"+line+"\n")

	if !strings.Contains(out, "empty_content") {
		t.Fatalf("NDJSON 空 choices 应判为空流, 实得:\n%s", out)
	}
}

// 合成的错误帧必须是合法 JSON 且带 finish_reason ——
// 只认协议终止信号、不解析 error 字段的客户端也能正常收尾。
func Test空流_错误帧协议合法性(t *testing.T) {
	out := drainStream(t, emptyChoicesFrame("1"))

	var found bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("错误帧必须是合法 JSON, 解析失败: %v, 原帧: %q", err, payload)
		}
		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if choice["finish_reason"] == "stop" {
			found = true
			errObj, _ := obj["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "empty_content" {
				t.Fatalf("错误帧应带 error.type=empty_content, 实得 %#v", obj["error"])
			}
			if model, _ := obj["model"].(string); model == "" {
				t.Fatal("错误帧应带上游 model 字段(便于定位)")
			}
		}
	}
	if !found {
		t.Fatalf("必须交付带 finish_reason=stop 的终止帧, 实得:\n%s", out)
	}
}

// nopCloser 把 strings.Reader 包成 ReadCloser 供 http.Response.Body 使用。
type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }
