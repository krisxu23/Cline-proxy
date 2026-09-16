package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 用例对应 OmniRoute stream.ts:261-339 + tests/unit/stream-utils.test.ts。

// ─────────────────── extractAllowedToolNames ───────────────────

func TestExtractAllowedToolNames_function包裹形态(t *testing.T) {
	// chat.completions 形态: {"type":"function","function":{"name":"Read",...}}
	tools := j(`[
		{"type":"function","function":{"name":"Read"}},
		{"type":"function","function":{"name":"Write"}}
	]`)
	got := extractAllowedToolNames(tools)
	if len(got) != 2 || !got["Read"] || !got["Write"] {
		t.Fatalf("应解析出 Read/Write, got %#v", got)
	}
}

func TestExtractAllowedToolNames_无包裹形态(t *testing.T) {
	// Responses 形态: 顶层直接是 name
	tools := j(`[{"type":"function","name":"Bash"}]`)
	got := extractAllowedToolNames(tools)
	if len(got) != 1 || !got["Bash"] {
		t.Fatalf("应解析出 Bash, got %#v", got)
	}
}

func TestExtractAllowedToolNames_function优先于顶层name(t *testing.T) {
	// 参考实现: `const name = functionName || directName;`
	tools := j(`[{"name":"outer","function":{"name":"inner"}}]`)
	got := extractAllowedToolNames(tools)
	if len(got) != 1 || !got["inner"] {
		t.Fatalf("应取 function.name 优先, got %#v", got)
	}
	if got["outer"] {
		t.Fatal("不应收录顶层 name")
	}
}

func TestExtractAllowedToolNames_过滤空名与非法条目(t *testing.T) {
	tools := j(`[
		null, "str", 42, [],
		{"function":{"name":"  "}},
		{"name":""},
		{"function":{"name":"Valid"}}
	]`)
	got := extractAllowedToolNames(tools)
	if len(got) != 1 || !got["Valid"] {
		t.Fatalf("只应留下 Valid, got %#v", got)
	}
}

func TestExtractAllowedToolNames_无tools返回nil(t *testing.T) {
	// 参考实现: `if (!Array.isArray(tools)) return null;`
	if got := extractAllowedToolNames(nil); got != nil {
		t.Fatalf("nil 应返回 nil, got %#v", got)
	}
	if got := extractAllowedToolNames(map[string]any{"tools": "not-an-array"}); got != nil {
		t.Fatalf("非数组应返回 nil, got %#v", got)
	}
}

func TestExtractAllowedToolNames_空数组返回nil(t *testing.T) {
	// 参考实现: `return names.size > 0 ? names : null;`
	if got := extractAllowedToolNames([]any{}); got != nil {
		t.Fatalf("空数组应返回 nil (不过滤), got %#v", got)
	}
}

func TestExtractAllowedToolNames_名称去除首尾空白(t *testing.T) {
	tools := j(`[{"function":{"name":"  Read  "}}]`)
	got := extractAllowedToolNames(tools)
	if !got["Read"] {
		t.Fatalf("应 trim 后收录 Read, got %#v", got)
	}
}

// ─────────────────── containsMalformedTextualToolCall ───────────────────

func TestMalformed_畸形形态判真(t *testing.T) {
	// 参考: stream-utils.test.ts:606 的紧凑畸形形态
	in := "[Tool call: search_files_ide{file_glob:*combos*.ts,path:/opt/OmniRoute,target:files}]"
	if !containsMalformedTextualToolCall(in, nil) {
		t.Fatal("头部合法但解析不出完整调用 → 应判真")
	}
}

func TestMalformed_白名单外的工具名判真(t *testing.T) {
	// 参考实现 :286-288 —— 解析成功但名字不在白名单 → true(幻觉调用)
	in := "[Tool call: evil_tool]\nArguments: {}"
	allowed := map[string]bool{"Read": true}
	if !containsMalformedTextualToolCall(in, allowed) {
		t.Fatal("白名单外的工具名应判真")
	}
}

func TestMalformed_白名单内的工具名判假(t *testing.T) {
	in := "[Tool call: Read]\nArguments: {}"
	allowed := map[string]bool{"Read": true}
	if containsMalformedTextualToolCall(in, allowed) {
		t.Fatal("白名单内且可解析 → 应判假")
	}
}

func TestMalformed_无白名单时不做名字校验(t *testing.T) {
	// 参考实现 :286 `allowedToolNames?.size && ...` —— 无白名单则不校验
	in := "[Tool call: anything]\nArguments: {}"
	if containsMalformedTextualToolCall(in, nil) {
		t.Fatal("无白名单时不应因名字判真")
	}
}

func TestMalformed_正文中的误报判假(t *testing.T) {
	// 参考: stream-utils.test.ts:2423
	in := "Checking: [Tool call: terminal] was executed successfully."
	if containsMalformedTextualToolCall(in, nil) {
		t.Fatal("误报场景应判假")
	}
}

func TestMalformed_多次扫描(t *testing.T) {
	// 参考实现用 while 循环逐个位置扫描(:278-295)
	// 第一处是误报(头部非法), 第二处才是畸形 → 应判真
	in := "See [Tool call: x] mention. Then [Tool call: broken{y}]"
	if !containsMalformedTextualToolCall(in, nil) {
		t.Fatal("应扫描到第二处畸形并判真")
	}
}

func TestMalformed_非字符串判假(t *testing.T) {
	for _, in := range []any{nil, 42, []any{"[Tool call: x]"}} {
		if containsMalformedTextualToolCall(in, nil) {
			t.Fatalf("非字符串应判假: %#v", in)
		}
	}
}

// ─────────────────── collectTextualToolCalls ───────────────────

func chunkWithContent(content string) map[string]any {
	return map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": content},
			},
		},
	}
}

func TestCollect_把文本调用转成tool_calls(t *testing.T) {
	// message(content=null) + tool_calls 是参考实现 :2530-2542 的终态
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent("[Tool call: Read]\nArguments: {\"path\":\"a.txt\"}")

	if !collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("应报告收集到工具调用")
	}
	if len(calls) != 1 {
		t.Fatalf("应收集 1 个调用, got %d", len(calls))
	}

	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	// :2526 —— 正文被摘除
	if delta["content"] != nil {
		t.Fatalf("正文应被摘除, got %#v", delta["content"])
	}
	tc, ok := delta["tool_calls"].([]any)
	if !ok || len(tc) != 1 {
		t.Fatalf("delta 应带 1 个 tool_calls, got %#v", delta["tool_calls"])
	}
	fn := tc[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Read" {
		t.Fatalf("工具名错误: %#v", fn["name"])
	}
	// arguments 必须是 JSON 字符串(参考实现 JSON.stringify)
	if s, ok := fn["arguments"].(string); !ok {
		t.Fatalf("arguments 应为字符串, got %T", fn["arguments"])
	} else {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			t.Fatalf("arguments 不是合法 JSON: %v", err)
		}
		if parsed["path"] != "a.txt" {
			t.Fatalf("arguments 内容错误: %#v", parsed)
		}
	}
}

func TestCollect_畸形内容被清空(t *testing.T) {
	// :2527-2528 —— 畸形标记必须清空正文, 否则 agent 当普通文本读
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent("[Tool call: broken{file:a}]")

	collectTextualToolCalls(chunk, calls, nil)

	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != nil {
		t.Fatalf("畸形内容应被清空, got %#v", delta["content"])
	}
	if len(calls) != 0 {
		t.Fatalf("不应收集到调用, got %#v", calls)
	}
}

func TestCollect_白名单外不收集但被判定畸形(t *testing.T) {
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent("[Tool call: evil]\nArguments: {}")
	allowed := map[string]bool{"Read": true}

	collectTextualToolCalls(chunk, calls, allowed)

	if len(calls) != 0 {
		t.Fatalf("白名单外不应收集, got %#v", calls)
	}
	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	// containsMalformedTextualToolCall 判真 → 内容被清空
	if delta["content"] != nil {
		t.Fatalf("白名单外调用应被清空, got %#v", delta["content"])
	}
}

func TestCollect_普通正文不受影响(t *testing.T) {
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent("这是一段普通回复, 没有任何工具调用。")

	if collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("普通正文不应报告工具调用")
	}
	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != "这是一段普通回复, 没有任何工具调用。" {
		t.Fatalf("普通正文不应被改动, got %#v", delta["content"])
	}
	if _, has := delta["tool_calls"]; has {
		t.Fatal("普通正文不应产生 tool_calls")
	}
}

func TestCollect_误报句子不被吞掉(t *testing.T) {
	// 关键回归: stream-utils.test.ts:2456 `assert.equal(choice.message.content, sentence)`
	sentence := "Checking: [Tool call: terminal] was executed successfully."
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent(sentence)

	collectTextualToolCalls(chunk, calls, nil)

	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != sentence {
		t.Fatalf("误报句子必须原样保留\n got: %#v\nwant: %q", delta["content"], sentence)
	}
}

func TestCollect_空choices不报错(t *testing.T) {
	calls := map[string]textualToolCallRecord{}
	chunk := map[string]any{"choices": []any{}}
	if collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("空 choices 应返回 false")
	}
}

func TestCollect_无content字段不报错(t *testing.T) {
	calls := map[string]textualToolCallRecord{}
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}},
	}
	if collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("无 content 应返回 false")
	}
}

func TestCollect_message形态也支持(t *testing.T) {
	// 非流式形态: choices[0].message 而非 delta
	calls := map[string]textualToolCallRecord{}
	chunk := map[string]any{
		"choices": []any{
			map[string]any{
				"index":   0,
				"message": map[string]any{"role": "assistant", "content": "[Tool call: Read]\nArguments: {}"},
			},
		},
	}
	if !collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("message 形态也应被支持")
	}
	msg := chunk["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("正文应被摘除, got %#v", msg["content"])
	}
}

func TestCollect_多轮调用累积且顺序稳定(t *testing.T) {
	calls := map[string]textualToolCallRecord{}
	collectTextualToolCalls(chunkWithContent("[Tool call: A]\nArguments: {}"), calls, nil)
	collectTextualToolCalls(chunkWithContent("[Tool call: B]\nArguments: {}"), calls, nil)

	if len(calls) != 2 {
		t.Fatalf("应累积 2 个调用, got %d", len(calls))
	}
	delta := textualToolCallsToDelta(calls)
	first := delta[0].(map[string]any)
	second := delta[1].(map[string]any)
	if first["index"] != 0 || second["index"] != 1 {
		t.Fatalf("顺序应稳定升序: %#v", delta)
	}
	if first["function"].(map[string]any)["name"] != "A" {
		t.Fatalf("第一个应是 A: %#v", first)
	}
}

func TestCollect_id与type字段齐全(t *testing.T) {
	// 参考实现 :329-331 —— id 形如 call_<ts>_<idx>, type 恒为 "function"
	calls := map[string]textualToolCallRecord{}
	collectTextualToolCalls(chunkWithContent("[Tool call: A]\nArguments: {}"), calls, nil)
	delta := textualToolCallsToDelta(calls)
	item := delta[0].(map[string]any)
	id, _ := item["id"].(string)
	if !strings.HasPrefix(id, "call_") {
		t.Fatalf("id 前缀应为 call_, got %q", id)
	}
	if item["type"] != "function" {
		t.Fatalf("type 应为 function, got %#v", item["type"])
	}
}

func TestCollect_零宽字符调用可被收集(t *testing.T) {
	// 上游把零宽字符混进工具名 —— 必须先清洗才能解析
	calls := map[string]textualToolCallRecord{}
	chunk := chunkWithContent("[Tool call: Rea\u200Dd]\nArguments: {}")
	if !collectTextualToolCalls(chunk, calls, nil) {
		t.Fatal("含零宽字符的调用应可被收集")
	}
	delta := textualToolCallsToDelta(calls)
	name := delta[0].(map[string]any)["function"].(map[string]any)["name"]
	if name != "Read" {
		t.Fatalf("工具名应被清洗为 Read, got %q", name)
	}
}

// ─────────────────── streamRequestTools / attach ───────────────────

func TestAttachStreamRequestTools_写入并可读回(t *testing.T) {
	body := map[string]any{"tools": j(`[{"function":{"name":"Read"}}]`)}
	req, _ := http.NewRequest("POST", "http://example.com", nil)
	attachStreamRequestTools(req, body)

	resp := &http.Response{Request: req}
	got := extractAllowedToolNames(streamRequestTools(resp))
	if len(got) != 1 || !got["Read"] {
		t.Fatalf("应读回 Read, got %#v", got)
	}
}

func TestAttachStreamRequestTools_无tools不写头(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://example.com", nil)
	attachStreamRequestTools(req, map[string]any{})
	if req.Header.Get(streamToolsHeaderName) != "" {
		t.Fatal("无 tools 时不应写入 header")
	}
	resp := &http.Response{Request: req}
	if got := streamRequestTools(resp); got != nil {
		t.Fatalf("应读回 nil, got %#v", got)
	}
}

func TestStreamRequestTools_无请求时返回nil(t *testing.T) {
	if got := streamRequestTools(nil); got != nil {
		t.Fatalf("nil response 应返回 nil, got %#v", got)
	}
	if got := streamRequestTools(&http.Response{}); got != nil {
		t.Fatalf("无 Request 应返回 nil, got %#v", got)
	}
}

func TestStreamRequestTools_坏JSON返回nil(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://example.com", nil)
	req.Header.Set(streamToolsHeaderName, "{not json")
	resp := &http.Response{Request: req}
	if got := streamRequestTools(resp); got != nil {
		t.Fatalf("坏 JSON 应返回 nil, got %#v", got)
	}
}

// ─────────────────── indexFrom ───────────────────

func TestIndexFrom_语义等价JS(t *testing.T) {
	h := "abcabc"
	cases := []struct {
		from, want int
	}{
		{0, 0}, {1, 3}, {3, 3}, {4, -1}, {100, -1}, {-5, 0},
	}
	for _, c := range cases {
		if got := indexFrom(h, "abc", c.from); got != c.want {
			t.Fatalf("indexFrom(%q, abc, %d) = %d, want %d", h, c.from, got, c.want)
		}
	}
}
