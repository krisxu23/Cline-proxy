package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mustParsedBody 把测试 JSON 字节解析成判定入参(fullCompletionBodyHasContent 吃已解析 map)。
func mustParsedBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	m, ok := parseJSONMap(body)
	if !ok {
		t.Fatalf("测试 JSON 解析失败: %s", body)
	}
	return m
}

// TestFullCompletionBodyArrayContent 完整 JSON body 的 content 若为数组形态
// (OpenAI content-parts / 翻译后的 blocks), 任一块带非空 text 即算有内容,
// 不能把正常回包误判成空壳打 502(2026-09-17 审查 R2-6)。
func TestFullCompletionBodyArrayContent(t *testing.T) {
	withText := []byte(`{"choices":[{"message":{"content":[{"type":"text","text":"hello"}]}}]}`)
	if !fullCompletionBodyHasContent(mustParsedBody(t, withText)) {
		t.Fatal("带非空 text 的 content 数组应判为有内容")
	}
	emptyBlocks := []byte(`{"choices":[{"message":{"content":[{"type":"text","text":""}]}}]}`)
	if fullCompletionBodyHasContent(mustParsedBody(t, emptyBlocks)) {
		t.Fatal("content 数组里全是空 text 不应判为有内容")
	}
	stringContent := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)
	if !fullCompletionBodyHasContent(mustParsedBody(t, stringContent)) {
		t.Fatal("字符串 content 应照旧判为有内容")
	}
}

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

// ─────────────────── chunkDeliversUserContent 判定（流级口径）───────────────────
//
// ★ 本测试于 2026-09-17 审查 P0-1 时**有意改了四处期望值**。
//
// 原测试锁定的是**帧级转发过滤**的判据(照抄 OmniRoute hasValuableContent),
// 那里 role / finish_reason 算"有价值"是对的 —— 客户端靠它们收尾, 这一帧
// 必须转发。但该函数被当成了**流级产出判据**, 于是这些脚手架帧能让空流
// 穿过守卫。现改用流级口径(chunkDeliversUserContent), 期望值随之调整。
//
// 详见 stream_delivery.go 顶部注释与 proxy_stream.go 里被删函数的说明。

func TestChunkDeliversUserContent(t *testing.T) {
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
			// ★ 改动 1/4: 原为 true(帧级判据接受 finish_reason)。
			// 流级口径下 finish_reason 是**脚手架**, 不是产出 ——
			// 一条只发 finish_reason 就结束的流对用户等于零产出, 必须判空。
			name: "带 finish_reason 但零产出 → 无价值(脚手架帧)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			want: false,
		},
		{
			// ★ 改动 2/4: 原为 false(帧级判据只认 delta)。
			// 流级口径也接受非流式的 message 形态 —— 完整回包合成路径用它承载正文,
			// 只认 delta 会让有内容的回包被判空。
			name: "非流式 message 形态带 content → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "message": map[string]any{"content": "hi"}},
			}},
			want: true,
		},
		{
			// ★ 改动 3/4: 原为 true(帧级判据显式接受 role)。
			// role 骨架帧是**脚手架**, 不是产出。
			name: "只有 role 骨架帧 → 无价值(脚手架帧)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}},
			}},
			want: false,
		},
		{
			name: "delta 带 reasoning_content → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "想一下"}},
			}},
			want: true,
		},
		{
			// ★ 改动 4/4: 原为 false(帧级判据只认 choices[0])。
			// 流级问题问的是"有没有产出", 任一 choice 有产出即算交付。
			name: "多 choice: 任一有内容即有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": ""}},
				map[string]any{"index": 1, "delta": map[string]any{"content": "有内容"}},
			}},
			want: true,
		},
		// ── 以下为流级口径新增的边界用例 ──
		{
			// reasoning 只发空白: 帧级判据不 trim(流式 delta 的首尾空格有意义),
			// 但流级口径要的是"用户能不能看到东西", 纯空白等于零产出。
			name: "reasoning 只有空格 → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "   "}},
			}},
			want: false,
		},
		{
			name: "reasoning 带内容与空格 → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": " 想 "}},
			}},
			want: true,
		},
		{
			name: "content 为数组且块带 text → 有价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "hi"}},
				}},
			}},
			want: true,
		},
		{
			name: "content 为数组但块全空 → 无价值",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{
					"content": []any{map[string]any{"type": "text", "text": ""}},
				}},
			}},
			want: false,
		},
		{
			name: "包了一层 data 的 choices 也能识别",
			obj: map[string]any{"data": map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}},
			}}},
			want: true,
		},
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
		{
			name: "nil → 无价值",
			obj:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkDeliversUserContent(tc.obj); got != tc.want {
				t.Fatalf("chunkDeliversUserContent = %v, 期望 %v (obj=%#v)", got, tc.want, tc.obj)
			}
		})
	}
}

// TestHasOutputUsageTokens 输出侧 token 判定: 输入侧有值不算"已交付"。
//
// 这是 P0-1 的第二处漏洞 —— 原判据(任一 token 字段 > 0)把
// {prompt_tokens:1500, completion_tokens:0} 这类真·空回包当成已交付。
func TestHasOutputUsageTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  bool
	}{
		{"只有 prompt_tokens → 不算产出", map[string]any{"prompt_tokens": float64(1500)}, false},
		{"只有 input_tokens → 不算产出", map[string]any{"input_tokens": float64(1500)}, false},
		{"只有 promptTokenCount → 不算产出", map[string]any{"promptTokenCount": float64(1500)}, false},
		{"只有 total_tokens → 不算产出", map[string]any{"total_tokens": float64(1500)}, false},
		{"completion_tokens>0 → 算产出", map[string]any{"prompt_tokens": float64(1500), "completion_tokens": float64(1)}, true},
		{"output_tokens>0 → 算产出", map[string]any{"output_tokens": float64(1)}, true},
		{"candidatesTokenCount>0 → 算产出", map[string]any{"candidatesTokenCount": float64(1)}, true},
		{"全零 → 不算产出", map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}, false},
		{"nil → 不算产出", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOutputUsageTokens(tc.usage); got != tc.want {
				t.Fatalf("hasOutputUsageTokens(%#v) = %v, 期望 %v", tc.usage, got, tc.want)
			}
		})
	}
}

// ─────────────────── 端到端: 真实 handleStreamResponseWithUsage ───────────────────

// drainStreamResult 用 httptest 驱动真实的流式处理函数, 返回 (交付状态码, 客户端
// 看到的全部字节)。上游 body 由 frames 顺序拼接而成。
//
// ★ 2026-09-24 契约变更(用户规格: 上游的错误必须在网关内部消化, 不得让 agent 看到):
// 空流在响应头**未提交**时不再交付任何字节, 而是返回真 502, 由 routing_dispatch.go
// 在网关内部冷却本站 + 换下一站。所以"有没有交付错误帧"这种旧断言不再充分 ——
// 必须同时看状态码与交付字节数。
func drainStreamResult(t *testing.T, upstreamBody string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}
	upstream.Body = nopCloser{strings.NewReader(upstreamBody)}
	status := handleStreamResponseWithUsage(rec, upstream, nil)
	return status, rec.Body.String()
}

// drainStream 只关心交付字节的调用方沿用旧签名。
func drainStream(t *testing.T, upstreamBody string) string {
	t.Helper()
	_, out := drainStreamResult(t, upstreamBody)
	return out
}

// assertInvisibleEmptyStream 断言"这条空流被网关**内部消化**": 客户端一个字节都
// 收不到, 调用方拿到真 502 去换下一站。
//
// 这是 2026-09-24 用户规格的落地 —— 上游的空回包不得让 agent 看到任何东西。旧行为
// (200 + 一条带 empty_content 错误帧的空流)在 agent 眼里就是"空白回复": 它认不出
// 那个自定义帧, 等不到内容就卡死。
//
// 断言**没有放松, 反而更严**: 旧用例只要求"必须出现 empty_content 帧"; 现在要求
// "零字节交付" **且** "返回 502" —— 两条都比原来更难满足。
func assertInvisibleEmptyStream(t *testing.T, body, why string) {
	t.Helper()
	status, out := drainStreamResult(t, body)
	if status != http.StatusBadGateway {
		t.Fatalf("%s: 空流必须以真 502 交还调用方(供路由层换站重试), 实得 status=%d, 交付 %d 字节:\n%s",
			why, status, len(out), out)
	}
	if out != "" {
		t.Fatalf("%s: 空流在响应头提交前不得向客户端交付任何字节, 实得 %d 字节:\n%s", why, len(out), out)
	}
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

// ── 空流用例的共同契约(2026-09-24 起) ──
//
// 对应用户规格: 上游返回的任何错误(429 / 403 / 502 / 空回包)都必须在网关**内部
// 消化**, 不得让 agent 看到; 网关自己换节点重试, 只有真实结果才回给 agent。
//
// 因此下面每一条"必须判空"的用例, 断言都从**旧**的
//     "必须交付一条 empty_content 错误帧(200 流内)"
// 改为**新**的
//     "一个字节都不交付 + 返回真 502"
// 理由逐条写在各自用例里 —— 共同点是: 只要响应头还没提交, 就没有理由把一个
// agent 认不出的自定义错误帧塞给它; 让路由层换一站拿到真实结果才是用户要的行为。
//
// 注意: "不交付任何字节"之所以能做到, 是因为处理链在见到第一个**真实内容帧**之前
// 一个字节都不提交(proxy_stream.go 的 lazyCommitWriter + markDelivered)——
// 脚手架帧(NDJSON 转发 / 合成器注入的 delta.role / 上游空 choices)只攒在前导缓冲里。

// 对应用例 1: "an all-empty-choices stream is rejected as a retryable error"。
// 参考断言: errored || !output.includes("[DONE]")。
// 本网关的等价契约: 绝不能出现"干净的空 200"—— 现在进一步做到客户端零感知。
func Test空流_全空choices必须失败(t *testing.T) {
	// ★ 契约变更理由: 旧契约是"必须交付 empty_content 错误帧"。但那条帧是网关
	// 自造的, agent 工具认不出它 —— 用户报的"空白回复卡死"正是这么来的。
	// 响应头此时尚未提交(整条流只有脚手架帧), 所以正确做法是零交付 + 502,
	// 由 routing_dispatch.go 换下一站。
	assertInvisibleEmptyStream(t, emptyChoicesFrame("1")+emptyChoicesFrame("2"),
		"全空 choices 流")
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

// role 骨架帧**不算**有价值内容。
//
// ★ 2026-09-17 审查 P0-1 修正: 原测试断言"骨架帧按 hasValuableContent 应算
// 有价值, 不应判空流"。那是**帧级转发过滤**的判据 —— role 帧必须转发给客户端
// (客户端靠它收尾), 所以帧级算"有价值"是对的。但把它当**流级产出判据**就错了:
// 一条只发了 role 骨架帧就 [DONE] 的流, 对用户等于零产出, 必须判空并换站。
//
// ★ 2026-09-24 契约变更: 判空后的交付形态从"200 流 + empty_content 错误帧"改为
// "零交付 + 真 502"。role 帧恰恰是**不能**触发提交的那种帧 —— 它若触发提交,
// 空流就再也换不了站, 本次修复当场作废。
func Test空流_仅骨架帧必须判空(t *testing.T) {
	skeleton := "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	assertInvisibleEmptyStream(t, skeleton+"data: [DONE]\n\n",
		"只有 role 骨架帧的流(骨架帧不是产出)")
}

// 只有 finish_reason 的流同样必须判空 —— 它是另一个脚手架帧。
func Test空流_仅finish_reason必须判空(t *testing.T) {
	assertInvisibleEmptyStream(t, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		"只有 finish_reason=stop 的流")
}

// 只有 finish_reason=length 的流**不应**判空 —— 它是"合法空终止态"
// (被 token 上限截断, 本就不该有正文)。这条防的是"收紧判据时误杀合法空回包"。
//
// ★ 2026-09-24 断言**加强**: 原来只断言"没有 empty_content 帧"。新契约下这条
// 会漏掉一种真回归 —— 合法空终止态走的是"收尾兜底提交"(见 lazyCommitWriter.commit
// 的第二个调用点), 那里若忘了提交, 客户端会收到一个 **200 却零字节**的响应,
// 而"没有 empty_content"照样成立。所以补上"必须真的交付终止帧 + [DONE]"。
func Test空流_finish_reason为length属合法空不判空(t *testing.T) {
	out := drainStream(t, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\n")

	if strings.Contains(out, "empty_content") {
		t.Fatalf("finish_reason=length 是合法空终止态, 不应判空流, 实得:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("合法空终止态的帧必须交付给客户端(否则是 200 零字节), 实得:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("合法空终止态必须收尾 [DONE], 实得:\n%s", out)
	}
}

// ★ 2026-09-17 审查 P0-1 修正: 原测试断言"带真实 usage 的流不应判空",
// 而输入是 {"usage":{"prompt_tokens":1},"choices":[]} —— 纯**输入侧** token、
// 零输出、choices 为空。这正是最典型的真·空回包, 却被当成正确行为守护着。
//
// 输入 token 有值只说明上游收到了 prompt, 完全不能说明它产出了东西。
func Test空流_仅输入侧usage必须判空(t *testing.T) {
	assertInvisibleEmptyStream(t, "data: "+`{"id":"c1","model":"m","usage":{"prompt_tokens":1},"choices":[]}`+"\n\n",
		"只有 prompt_tokens(输入侧)的流")
}

// 只有**输出侧** token 的流是合法交付 —— 上游确实生成了东西(可能因
// finish_reason=length 被截断成 0 字符, 但 completion_tokens 有值)。
//
// ★ 2026-09-24 断言**加强**: 与上面 length 用例同理 —— 必须真的交付, 不能只是
// "没报错"(忘掉收尾兜底提交会退化成 200 零字节)。
func Test空流_输出侧usage算有效交付(t *testing.T) {
	out := drainStream(t, "data: "+`{"id":"c1","model":"m","usage":{"prompt_tokens":10,"completion_tokens":5},"choices":[]}`+"\n\n")

	if strings.Contains(out, "empty_content") {
		t.Fatalf("带输出侧 token 的流不应判空, 实得:\n%s", out)
	}
	if !strings.Contains(out, `"completion_tokens":5`) {
		t.Fatalf("带输出侧 token 的帧必须交付给客户端(否则是 200 零字节), 实得:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("带输出侧 token 的流必须收尾 [DONE], 实得:\n%s", out)
	}
}

// 全零 usage 是空壳, 不算有效用量。
func Test空流_全零usage不算有效(t *testing.T) {
	assertInvisibleEmptyStream(t, "data: "+`{"id":"c1","model":"m","usage":{"prompt_tokens":0,"completion_tokens":0},"choices":[]}`+"\n\n",
		"全零 usage 的空壳流")
}

// reasoning 只发空白: 流级口径要的是"用户能不能看到东西", 纯空白等于零产出。
func Test空流_仅空格reasoning必须判空(t *testing.T) {
	assertInvisibleEmptyStream(t, "data: "+`{"choices":[{"index":0,"delta":{"reasoning_content":"   "}}]}`+"\n\n",
		"只有空白 reasoning 的流")
}

// 关键回归: 上游一帧内容都不给、直接发 [DONE]。
// 网关会为缺少 finish_reason 的上游**自己合成**一个终止帧 —— 那个合成帧是
// 脚手架, 绝不能被算成"上游交付了有价值内容", 否则这条最典型的静默中断
// (上游空手而归 + [DONE]) 会绕过空流防护, 退回成干净的空 200。
func Test空流_只发DONE不合成价值(t *testing.T) {
	assertInvisibleEmptyStream(t, "data: [DONE]\n\n",
		"上游只发 [DONE](网关自补的终止帧不算产出)")
}

// 同上, 但上游先发若干空 choices 帧再发 [DONE]。
func Test空流_空choices后接DONE必须失败(t *testing.T) {
	assertInvisibleEmptyStream(t, emptyChoicesFrame("1")+"data: [DONE]\n\n",
		"空 choices + [DONE]")
}

// 合成路径 1: 完整 JSON body 回空壳 choices —— 必须同样被判空流。
// (这是自查发现的缺陷: 合成路径早期绕过了空流判定。)
//
// ★ 2026-09-24 契约变更的理由在这条上最直观: 旧行为会先 `hb.writeFlush(sse)`
// 把合成帧(含注入的 delta.role 脚手架)写出去、把 200 提交掉, 然后才发现 body
// 是空壳 —— 客户端因此拿到"200 + 一条它认不出的错误帧"。现在改为**先按原始
// body 判定并提交、再写合成帧**, 空壳 body 一帧都不写。
func Test空流_完整JSON空壳body必须失败(t *testing.T) {
	// 把 idle 超时压到 1s: 完整 JSON 路径要读到底才能确认 body 形态, 而"body 不带
	// 换行"时会等满 idle 超时(默认 90s)。那是改动前就存在的现象(本用例不关心超时),
	// 不压的话单条用例要跑 90 秒 —— 三条合计 270s, 占了整个 internal/app 套件的九成
	// 时间(2026-09-24 审计 P0-1)。
	withTestConfig(t, &zenConfigData{StreamIdleSecs: 1})
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
	assertInvisibleEmptyStream(t, body, "完整 JSON 空壳 body")
}

// 合成路径 2: 完整 JSON body 带真实内容 —— 必须正常通过(负向对照)。
func Test空流_完整JSON有内容正常通过(t *testing.T) {
	// 把 idle 超时压到 1s: 完整 JSON 路径要读到底才能确认 body 形态, 而"body 不带
	// 换行"时会等满 idle 超时(默认 90s)。那是改动前就存在的现象(本用例不关心超时),
	// 不压的话单条用例要跑 90 秒 —— 三条合计 270s, 占了整个 internal/app 套件的九成
	// 时间(2026-09-24 审计 P0-1)。
	withTestConfig(t, &zenConfigData{StreamIdleSecs: 1})
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"real answer"},"finish_reason":"stop"}]}`
	out := drainStream(t, body)

	if !strings.Contains(out, "real answer") {
		t.Fatalf("合成路径的真实内容必须被转发, 实得:\n%s", out)
	}
	if strings.Contains(out, "empty_content") {
		t.Fatalf("有内容的完整 JSON 不得判为空流, 实得:\n%s", out)
	}
	// 合成器注入的 role 脚手架帧在前导缓冲里, 必须在提交时**先于**正文原样放出,
	// 否则客户端会收到一条缺头少尾的流(这正是"缓冲而不是丢弃"的理由)。
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("前导脚手架帧必须在提交时一并交付(否则缺头), 实得:\n%s", out)
	}
}

// 合成路径 3: NDJSON 逐行空 choices —— 必须被判空流。
func Test空流_NDJSON空choices必须失败(t *testing.T) {
	line := `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[]}`
	assertInvisibleEmptyStream(t, line+"\n"+line+"\n", "NDJSON 空 choices 流")
}

// ★ 2026-09-24 新增(自查发现的漏洞): 完整 JSON 形态的**合法空终止态**
// (finish_reason=length, 零正文)必须正常通过, 而且必须**真的交付**。
//
// 这条路径是提前 return、不经过收尾的兜底提交, 所以"没有 empty_content"这个旧
// 断言完全盖不住它: 漏掉那次 commit 就会退化成 200 零字节 —— 比空流更隐蔽。
func Test空流_完整JSON合法空终止态必须交付(t *testing.T) {
	// 把 idle 超时压到 1s: 完整 JSON 路径要读到底才能确认 body 形态, 而这条路径
	// 在"body 不带换行"时会等满 idle 超时(默认 90s)。那是改动前就存在的现象
	// (本用例不关心超时, 只关心有没有真的交付), 不压的话单条用例要跑 90 秒。
	withTestConfig(t, &zenConfigData{StreamIdleSecs: 1})
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`
	out := drainStream(t, body)

	if strings.Contains(out, "empty_content") {
		t.Fatalf("finish_reason=length 是合法空终止态, 不得判空流, 实得:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("合法空终止态的终止帧必须真的交付(否则是 200 零字节), 实得:\n%s", out)
	}
}

// ★ 2026-09-24 新增(同上): NDJSON 形态只带**输出侧 usage**、零正文时也必须真的交付。
func Test空流_NDJSON仅输出侧usage必须交付(t *testing.T) {
	line := `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","usage":{"completion_tokens":5},"choices":[]}`
	out := drainStream(t, line+"\n")

	if strings.Contains(out, "empty_content") {
		t.Fatalf("带输出侧 token 的 NDJSON 流不得判空, 实得:\n%s", out)
	}
	if !strings.Contains(out, `"completion_tokens":5`) {
		t.Fatalf("输出侧 usage 帧必须真的交付(否则是 200 零字节), 实得:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("必须收尾 [DONE], 实得:\n%s", out)
	}
}

// 合成的错误帧必须是合法 JSON 且带 finish_reason ——
// 只认协议终止信号、不解析 error 字段的客户端也能正常收尾。
//
// ★ 2026-09-24 契约变更: 空流在响应头**未提交**时已经零交付(见
// assertInvisibleEmptyStream), 这条帧不再出现在正常空流路径上。它仍是
// "响应头已提交后才发现空流"的兜底(见 writeStreamEmptyContentError 的注释)。
// 因此改为**直接驱动帧构造器** —— 断言一条不减(合法 JSON / 非 stop 终止帧 /
// error.type=empty_content / retryable / 带 model), 只是不再依赖那条按当前代码
// 已不可达的 handler 分岔。
func Test空流_错误帧协议合法性(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lazyCommitWriter{ResponseWriter: rec, flusher: rec}
	// interval=0: 不起心跳泵(本用例只关心错误帧的字节形态)。
	hb := newSSEHeartbeat(lw, lw, 0, func() []byte { return nil })
	defer hb.Close()
	lw.WriteHeader(http.StatusOK)
	lw.commit() // 已提交: 此时 HTTP 状态码改不了, 失败信息只能走 SSE 帧
	writeStreamEmptyContentError(lw, hb, "mimo-test")
	out := rec.Body.String()

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
		// 空流错误帧不得用 finish_reason=stop(2026-09-24 空 200 事件):
		// stop 是正常收尾语义, 客户端会当合法回合静默结束任务而不重试。
		if fr, _ := choice["finish_reason"].(string); fr != "" && fr != "stop" {
			found = true
			errObj, _ := obj["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "empty_content" {
				t.Fatalf("错误帧应带 error.type=empty_content, 实得 %#v", obj["error"])
			}
			if errObj["retryable"] != true {
				t.Fatalf("空流错误帧必须标 retryable=true, 实得 %#v", obj["error"])
			}
			if model, _ := obj["model"].(string); model == "" {
				t.Fatal("错误帧应带上游 model 字段(便于定位)")
			}
		}
	}
	if !found {
		t.Fatalf("必须交付非 stop 终止帧(空流不得静默收尾), 实得:\n%s", out)
	}
}

// nopCloser 把 strings.Reader 包成 ReadCloser 供 http.Response.Body 使用。
type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// ─────────── 合法空终止态白名单 ───────────
//
// 照抄 OmniRoute open-sse/utils/streamReadiness.ts:166 LEGIT_EMPTY_TERMINAL_REASONS,
// 权威来源 open-sse/services/errorClassifier.ts:14-15:
//
//	const LEGIT_EMPTY_CLAUDE_STOP = new Set(["max_tokens", "tool_use"]);
//	const LEGIT_EMPTY_OPENAI_FINISH = new Set(["length", "tool_calls", "content_filter"]);
//
// 参考用例: tests/unit/empty-stream-no-content-8649.test.ts
//   — "an all-empty-choices stream is rejected" 与
//     "a response truncated at the token limit is NOT flagged as empty content"
//
// 为什么必须抄: agent 工具调用回合**本来就只有 tool_calls 而无正文文本**,
// 被 token 上限截断的回合也**本来就没有正文**。误判成空流会把这些合法的成功
// 完成改写成合成的 502 —— 表现为 agent 的每次工具调用都失败重试。

func TestLegitEmptyTerminalReason(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{
			name: "finish_reason=length → 合法空终止(被 token 上限截断)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "length"},
			}},
			want: true,
		},
		{
			name: "finish_reason=tool_calls → 合法空终止(纯工具调用回合)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"},
			}},
			want: true,
		},
		{
			name: "finish_reason=content_filter → 合法空终止",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "content_filter"},
			}},
			want: true,
		},
		{
			name: "顶层 stop_reason=max_tokens → 合法空终止(Claude 形态)",
			obj:  map[string]any{"stop_reason": "max_tokens"},
			want: true,
		},
		{
			name: "顶层 stop_reason=tool_use → 合法空终止(Claude 形态)",
			obj:  map[string]any{"stop_reason": "tool_use"},
			want: true,
		},
		{
			name: "finish_reason=stop → 不在白名单(普通终止, 空内容仍是故障)",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			want: false,
		},
		{
			name: "无 finish_reason → 不在白名单",
			obj: map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}},
			}},
			want: false,
		},
		{
			name: "空 choices → 不在白名单",
			obj:  map[string]any{"choices": []any{}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legitEmptyTerminalReason(tc.obj); got != tc.want {
				t.Fatalf("legitEmptyTerminalReason() = %v, 期望 %v (obj=%#v)", got, tc.want, tc.obj)
			}
		})
	}
}

// ── 白名单真正起作用的地方: 只有终止原因、别无其他价值信号的帧 ──
//
// 负向验证得出的关键区分(重要!):
//   - 带 delta.tool_calls 的帧 → hasValuableContent 已判真, 白名单是**冗余**的;
//   - 带 firstChoice.finish_reason 的帧 → hasValuableContent 已判真, 白名单也是**冗余**的;
//   - **只有顶层 stop_reason 的 Claude 形态帧** → hasValuableContent 不认
//     (它只看 choices[0].delta.* 与 firstChoice.finish_reason),
//     此时**只有白名单能救它**。
//
// 实测证据: 把 legitEmptyTerminalReason(normalized) 改成 `&& false` 后,
// 本用例由"通过"变为"被判空流"; 带 tool_calls / finish_reason 的用例则**不受影响**。
// 这说明白名单并非装饰, 而是 Claude 形态空终止的唯一防线。
func Test空流_仅顶层stop_reason必须通过(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "max_tokens(被上限截断)",
			body: "data: " + `{"type":"message_delta","stop_reason":"max_tokens"}` + "\n\n",
		},
		{
			name: "tool_use(纯工具调用回合)",
			body: "data: " + `{"type":"message_delta","stop_reason":"tool_use"}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := drainStream(t, tc.body)
			if strings.Contains(out, "empty_content") {
				t.Fatalf("顶层 stop_reason=%s 是合法空终止态, 不得报空流 "+
					"(chunkDeliversUserContent 不认顶层 stop_reason, 此处唯一防线就是白名单), 实得:\n%s",
					tc.name, out)
			}
			// ★ 2026-09-24 断言加强: 合法空终止态靠"收尾兜底提交"才能交付, 忘掉
			// 提交就会退化成 200 零字节 —— 而"没有 empty_content"照样成立。
			if !strings.Contains(out, "[DONE]") {
				t.Fatalf("顶层 stop_reason=%s 必须真的交付收尾帧(否则是 200 零字节), 实得:\n%s",
					tc.name, out)
			}
		})
	}
}

// 对照: 顶层 stop_reason 取值**不在**白名单里时, 仍必须判空流。
// 防止"把整个 stop_reason 判真"这种过度放行。
func Test空流_顶层stop_reason非白名单值仍失败(t *testing.T) {
	body := "data: " + `{"type":"message_delta","stop_reason":"end_turn"}` + "\n\n"
	assertInvisibleEmptyStream(t, body, "顶层 stop_reason=end_turn(不在白名单)")
}

// 纯工具调用回合(无任何正文文本, finish_reason=tool_calls)必须正常通过,
// 不得被判空流。这正是 agent 工具调用场景, 误杀会让 agent 每次调工具都失败。
func Test空流_纯工具调用回合必须通过(t *testing.T) {
	body := "data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := drainStream(t, body)

	if strings.Contains(out, "empty_content") {
		t.Fatalf("纯工具调用回合是合法完成, 不得报空流, 实得:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Fatalf("工具调用帧必须转发给客户端, 实得:\n%s", out)
	}
}

// 被 token 上限截断的回合(finish_reason=length, 无正文)必须正常通过。
// OmniRoute 参考用例明确要求: "a response truncated at the token limit is
// NOT flagged as empty content"。
func Test空流_被token上限截断必须通过(t *testing.T) {
	body := "data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := drainStream(t, body)

	if strings.Contains(out, "empty_content") {
		t.Fatalf("被 token 上限截断是合法终止, 不得报空流, 实得:\n%s", out)
	}
	// ★ 2026-09-24 断言加强: 必须真的交付(收尾兜底提交一旦缺失就是 200 零字节)。
	if !strings.Contains(out, `"finish_reason":"length"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("合法截断的终止帧与 [DONE] 必须真的交付, 实得:\n%s", out)
	}
}

// 完整 JSON body 形态的纯工具调用回合(非流式回包被当成流式处理)也必须通过。
func Test空流_完整JSON纯工具调用必须通过(t *testing.T) {
	// 把 idle 超时压到 1s: 完整 JSON 路径要读到底才能确认 body 形态, 而"body 不带
	// 换行"时会等满 idle 超时(默认 90s)。那是改动前就存在的现象(本用例不关心超时),
	// 不压的话单条用例要跑 90 秒 —— 三条合计 270s, 占了整个 internal/app 套件的九成
	// 时间(2026-09-24 审计 P0-1)。
	withTestConfig(t, &zenConfigData{StreamIdleSecs: 1})
	body := `{"id":"c1","object":"chat.completion","model":"mimo-test","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	out := drainStream(t, body)

	if strings.Contains(out, "empty_content") {
		t.Fatalf("完整 JSON 形态的纯工具调用回合不得报空流, 实得:\n%s", out)
	}
	// ★ 2026-09-24 断言加强: 工具调用必须真的到达客户端。
	if !strings.Contains(out, "list_dir") {
		t.Fatalf("工具调用必须真的交付给客户端, 实得:\n%s", out)
	}
}

// 对照用例: 说明 **"转发" 与 "交付" 是两件事** —— 这是 P0-1 的核心。
//
// ★ 2026-09-17 审查修正。本用例原先断言"finish_reason=stop + 空 delta 不应判空流",
// 理由是"照抄 hasValuableContent(streamHelpers.ts:379) 的必然结果, 且 OmniRoute
// 自己的参考用例从不用它构造空流场景"。**这两条理由都成立, 但结论是错的** ——
// 因为它们证明的是"这个函数用于帧级转发过滤时, finish_reason 算有价值", 而
// 本网关把它用在了**流级产出判定**上。
//
// 参考实现为什么不需要处理"整条流零产出"?
//
//	因为 OmniRoute 靠**异常与超时**兜底(见 streaming_handler 的异常路径),
//	它的 hasValuableContent 只回答"这一帧要不要发给客户端"。
//
// 本网关把它升级成流级判据之后, 语义就变了:
//
//	帧级: finish_reason 帧 → 有价值 → **必须转发**(客户端靠它收尾)  ← 仍然成立
//	流级: finish_reason 帧 → 不是产出 → **不算交付**              ← 本用例锁定的新语义
//
// ★ 2026-09-24 契约变更(必须改输入, 不能只改断言):
// 旧用例的输入是"**只有** finish_reason 一帧"。新契约下那种流整条判空, 于是客户端
// 零交付(见 assertInvisibleEmptyStream)—— 那时"终止帧要转发"根本无从成立, 因为
// 一个字节都不会发出去。而"转发 ≠ 交付"这条不变量本身仍然正确, 只是它只在
// **已经交付过真实内容**的流上才可观测。所以输入改为"先有正文、后到终止帧":
// 终止帧不是产出(不改变流级判定), 但必须原样转发。
func Test空流_内容之后的终止帧仍必须转发(t *testing.T) {
	body := contentFrame("hi") +
		"data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := drainStream(t, body)

	// ① 流级: 有真实产出 → 不得判空
	if strings.Contains(out, "empty_content") {
		t.Fatalf("有真实内容的流不得判空, 实得:\n%s", out)
	}
	// ② 帧级: 终止帧**必须转发**给客户端 —— 收紧流级判据不得影响转发
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("终止帧必须转发给客户端(转发 ≠ 交付), 实得:\n%s", out)
	}
	// ③ 上游已发过 [DONE] 时收尾不得重复补(首行分支曾漏更新 sawDone)
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("[DONE] 应只出现一次, got %d in:\n%s", n, out)
	}
}

// 反向保护: 收紧流级判据**不得误杀**合法空回包。
// finish_reason=length 表示被 token 上限截断, 本就不该有正文, 是合法成功。
func Test空流_length截断的合法空回包不判空(t *testing.T) {
	body := "data: " + `{"id":"c1","object":"chat.completion.chunk","model":"mimo-test","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := drainStream(t, body)

	if strings.Contains(out, "empty_content") {
		t.Fatalf("finish_reason=length 是合法空终止态, 不得判空流, 实得:\n%s", out)
	}
	// ★ 2026-09-24 断言加强: 必须真的交付, 不能只是"没报错"。
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("合法截断的终止帧必须交付给客户端(否则是 200 零字节), 实得:\n%s", out)
	}
}

// 与上面对照: choices 为空数组才是参考实现定义的"空流", 必须失败。
// (emptyChoicesChunk 的等价物, 见 stream-empty-choices-interceptor.test.ts)
func Test空流_空choices数组才是真空流(t *testing.T) {
	body := emptyChoicesFrame("1") + emptyChoicesFrame("2") + "data: [DONE]\n\n"
	assertInvisibleEmptyStream(t, body, "choices 为空数组(参考实现定义的空流)")
}

// ─────────── lazyCommitWriter 的两条关键不变量(第二步的核心) ───────────

// TestLazyCommitWriter_提交前写出不得触发提交
//
// 这是"空流可换站"能否成立的关键不变量: 心跳走 hb.writeLocked → w.Write, 脚手架帧
// 同样走 w.Write。若 Write 沿用"首次写出即提交", 它们会把 200 钉死, 整个修复当场
// 作废。⇒ **提交必须是显式的**(处理链见到第一个真实内容帧时调 commit)。
func TestLazyCommitWriter_提交前写出不得触发提交(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lazyCommitWriter{ResponseWriter: rec, flusher: rec}
	lw.WriteHeader(http.StatusOK)
	// interval=0: 不起泵, 手动走与 pump 完全相同的那条写路径。
	hb := newSSEHeartbeat(lw, lw, 0, func() []byte { return openAIHeartbeatFrame })
	defer hb.Close()

	hb.writeFlush(openAIHeartbeatFrame)
	hb.writeFlush([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	if lw.isCommitted() {
		t.Fatal("写出本身不得触发提交(提交必须显式), 否则心跳/脚手架帧会把 200 钉死")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("提交前不得向客户端写出任何字节, 实得 %d 字节", rec.Body.Len())
	}

	// 显式提交: 前导缓冲里的帧必须按写入顺序一并放出, 且此后写出直通。
	lw.commit()
	if !lw.isCommitted() {
		t.Fatal("commit 之后必须处于已提交状态")
	}
	out := rec.Body.String()
	// 前导帧 = openAIHeartbeatFrame(`data: {"choices":[{"index":0,"delta":{}}]}`)
	if !strings.Contains(out, `"delta":{}`) {
		t.Fatalf("前导缓冲里的帧必须在提交时一并放出(不得丢弃), 实得 %q", out)
	}
	if strings.Index(out, `"delta":{}`) > strings.Index(out, `"content":"hi"`) {
		t.Fatalf("前导帧必须先于正文出现, 实得 %q", out)
	}
}

// TestLazyCommitWriter_前导超限绝不提交 前导缓冲超限时必须丢弃前导、拒绝写出,
// 且**绝不提交**响应头 —— 只有这样调用方才能返回真 502 让路由层换站。
//
// 反例(会被本用例抓住): 超限时改为"提交后放行"。那样响应头被钉死, 路由层再换
// 一站就会把第二次的内容接在第一次已经发出去的流后面 —— 用户明确警告过的顺序陷阱。
func TestLazyCommitWriter_前导超限绝不提交(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lazyCommitWriter{ResponseWriter: rec, flusher: rec}
	lw.WriteHeader(http.StatusOK)

	chunk := bytes.Repeat([]byte("x"), 64<<10)
	var lastErr error
	for i := 0; i < 64 && lastErr == nil; i++ { // 最多灌 4MiB, 上限 1MiB
		_, lastErr = lw.Write(chunk)
	}
	if lastErr == nil {
		t.Fatalf("超过 streamPreambleMaxBytes(%d) 后 Write 必须报错", streamPreambleMaxBytes)
	}
	if lw.isCommitted() {
		t.Fatal("前导超限后绝不允许提交响应头")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("前导超限不得向客户端写出任何字节, 实得 %d 字节", rec.Body.Len())
	}
	// 超限后 commit 必须是 no-op —— 否则调用方会误以为还能交付。
	lw.commit()
	if lw.isCommitted() || rec.Body.Len() != 0 {
		t.Fatalf("超限后 commit 必须 no-op, committed=%v bytes=%d", lw.isCommitted(), rec.Body.Len())
	}
	if !lw.isOverflowed() {
		t.Fatal("超限标志必须置位, 供处理链改判 502")
	}
}

// 正常路径的负向对照: 未超限时前导缓冲原样保留, 提交后按写入顺序交付。
func TestLazyCommitWriter_正常提交按序交付(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lazyCommitWriter{ResponseWriter: rec, flusher: rec}
	lw.WriteHeader(http.StatusOK)

	if _, err := lw.Write([]byte("A")); err != nil {
		t.Fatalf("提交前的写出不得报错: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("提交前不得有字节到达客户端, 实得 %q", rec.Body.String())
	}
	lw.commit()
	if _, err := lw.Write([]byte("B")); err != nil {
		t.Fatalf("提交后的写出不得报错: %v", err)
	}
	if got := rec.Body.String(); got != "AB" {
		t.Fatalf("前导 + 正文必须按写入顺序交付, 实得 %q", got)
	}
}
