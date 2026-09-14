package app

// zen Responses 适配层的单元测试: 请求/响应双向转换、SSE 流式翻译、
// 模型 API 形态登记(学习+持久化)。

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"cline-go-proxy/internal/kit"
)

func TestChatBodyToResponsesBody_Basic(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "and "},
				map[string]any{"type": "text", "text": "bye"},
			}},
		},
		"max_tokens":     64,
		"temperature":    0.7,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true}, // 应被丢弃
	}
	out := chatBodyToResponsesBody(chat)
	if out["model"] != "muse-spark-1.3-contributor-free" {
		t.Fatalf("model 透传失败: %v", out["model"])
	}
	if out["instructions"] != "You are helpful." {
		t.Fatalf("system 应转为 instructions: %v", out["instructions"])
	}
	if out["max_output_tokens"] != 64 {
		t.Fatalf("max_tokens 应转 max_output_tokens=64, got %v", out["max_output_tokens"])
	}
	if out["stream"] != true {
		t.Fatal("stream=true 应保留")
	}
	if _, has := out["stream_options"]; has {
		t.Fatal("stream_options 不应透传到 Responses 请求")
	}
	input, _ := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input 应为 3 条(2 user + 1 assistant), got %d", len(input))
	}
	first := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Fatalf("第一条 user 消息不符: %v", first)
	}
	last := input[2].(map[string]any)
	if last["content"] != "and bye" {
		t.Fatalf("分块 content 应拼成纯文本: %v", last["content"])
	}
}

func TestChatBodyToResponsesBody_ToolCalls(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "weather?"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{
					"id":       "call_1",
					"type":     "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"SG"}`},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": `{"temp":30}`},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "get_weather", "description": "query weather",
				"parameters": map[string]any{"type": "object"},
			}},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	}
	out := chatBodyToResponsesBody(chat)
	input := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("user+function_call+function_call_output 应为 3 条, got %d: %v", len(input), input)
	}
	fc := input[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" || fc["arguments"] != `{"city":"SG"}` {
		t.Fatalf("assistant tool_calls 应转为 function_call 项: %v", fc)
	}
	fco := input[2].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != `{"temp":30}` {
		t.Fatalf("tool 消息应转为 function_call_output: %v", fco)
	}
	tools := out["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["name"] != "get_weather" || t0["type"] != "function" {
		t.Fatalf("tools 应扁平化: %v", t0)
	}
	tc := out["tool_choice"].(map[string]any)
	if tc["name"] != "get_weather" {
		t.Fatalf("tool_choice 应扁平化 name: %v", tc)
	}
}

func TestResponsesToChatBody_Text(t *testing.T) {
	resp := map[string]any{
		"id":         "resp_1",
		"created_at": int64(1789362831),
		"model":      "muse-spark-1.3-contributor-free",
		"output": []any{
			map[string]any{"type": "reasoning", "content": []any{}},
			map[string]any{"type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": "你"},
				map[string]any{"type": "output_text", "text": "好"},
			}},
		},
		"usage": map[string]any{"input_tokens": 13, "output_tokens": 64, "total_tokens": 77},
	}
	out := responsesToChatBody(resp)
	if out["object"] != "chat.completion" || out["id"] != "resp_1" {
		t.Fatalf("外层字段不符: %v", out)
	}
	choices := out["choices"].([]any)
	c0 := choices[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Fatalf("finish_reason 应为 stop: %v", c0["finish_reason"])
	}
	msg := c0["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("output_text 应拼接: %v", msg["content"])
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != 13 || usage["completion_tokens"] != 64 {
		t.Fatalf("usage 映射错误: %v", usage)
	}
}

func TestResponsesToChatBody_ToolCallAndLength(t *testing.T) {
	resp := map[string]any{
		"id": "resp_2", "model": "m",
		"output": []any{
			map[string]any{"type": "function_call", "call_id": "c9", "name": "f", "arguments": `{"a":1}`},
		},
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
		"usage":              map[string]any{"input_tokens": 5, "output_tokens": 9, "total_tokens": 14},
	}
	out := responsesToChatBody(resp)
	c0 := out["choices"].([]any)[0].(map[string]any)
	// tool_calls 优先于 length
	if c0["finish_reason"] != "tool_calls" {
		t.Fatalf("有 tool_calls 时 finish_reason 应为 tool_calls: %v", c0["finish_reason"])
	}
	msg := c0["message"].(map[string]any)
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "c9" || tc["type"] != "function" {
		t.Fatalf("tool_calls 映射错误: %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("function 映射错误: %v", fn)
	}
}

func TestTranslateResponsesStreamToChat(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"你"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"好"}`,
		``,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"it1","call_id":"c1","name":"f"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"it1","delta":"{\"a\""}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"it1","delta":":1}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := translateResponsesStreamToChat(strings.NewReader(sse), &out, "m"); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	text := out.String()
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("应以 [DONE] 收尾, 尾部: %q", text[max(0, len(text)-40):])
	}
	var content strings.Builder
	toolArgs := strings.Builder{}
	sawFinish := false
	var usage map[string]any
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(line[6:]), &chunk) != nil {
			t.Fatalf("chat 分块不是合法 JSON: %q", line)
		}
		choices := chunk["choices"].([]any)
		c0 := choices[0].(map[string]any)
		if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
			sawFinish = true
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		delta := c0["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, ti := range tcs {
				tm := ti.(map[string]any)
				if fn, ok := tm["function"].(map[string]any); ok {
					if a, ok := fn["arguments"].(string); ok {
						toolArgs.WriteString(a)
					}
				}
			}
		}
	}
	if content.String() != "你好" {
		t.Fatalf("文本增量应按序拼接为 你好, got %q", content.String())
	}
	if toolArgs.String() != `{"a":1}` {
		t.Fatalf("函数参数增量应拼接, got %q", toolArgs.String())
	}
	if !sawFinish {
		t.Fatal("缺少 finish_reason 收尾块")
	}
	if usage == nil || usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(20) {
		t.Fatalf("usage 未随完成事件透出: %v", usage)
	}
}

func TestZenStaticResponsesOnlyRule(t *testing.T) {
	yes := []string{
		"muse-spark-1.3-contributor-free",
		"muse-spark-1.2-contributor-free",
		"muse-glimmer-free",
	}
	for _, m := range yes {
		if !zenStaticResponsesOnly(m) {
			t.Fatalf("%s 应命中静态规则", m)
		}
		if !zenUseResponsesAPI(m) {
			t.Fatalf("%s 应直接走 Responses 通道(无需探测)", m)
		}
	}
	no := []string{
		"mimo-v2.5-free", // 其他 -free 模型在 chat 上正常, 不能一刀切
		"deepseek-v4-flash-free",
		"muse-spark-1.3", // muse 付费版在 chat 上有正常路由
		"muse-spark-1.2",
		"claude-sonnet-4-6",
	}
	for _, m := range no {
		if zenStaticResponsesOnly(m) {
			t.Fatalf("%s 不应命中静态规则", m)
		}
	}
}

func TestZenResponsesFlavorLearnAndPersist(t *testing.T) {
	// 重定向数据文件到临时目录, 避免污染真实 data/
	origFile := zenResponsesOnlyFile()
	tmp := t.TempDir() + "/zen-responses-only.json"
	_ = os.Remove(tmp)
	setZenResponsesOnlyFileForTest(tmp)
	defer setZenResponsesOnlyFileForTest(origFile)

	if zenUseResponsesAPI("flavor-test-free") {
		t.Fatal("未登记的模型不应启用 Responses 通道")
	}
	zenLearnResponsesOnly("flavor-test-free")
	if !zenUseResponsesAPI("flavor-test-free") {
		t.Fatal("登记后应命中")
	}
	// 模拟进程重启: 强制重新从磁盘加载
	zenRespOnlyMu.Lock()
	zenRespOnly = map[string]bool{}
	zenRespOnlyLoaded = false
	zenRespOnlyMu.Unlock()
	if !zenUseResponsesAPI("flavor-test-free") {
		t.Fatal("持久化文件应在重新加载后仍命中")
	}
	raw, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("持久化文件缺失: %v", err)
	}
	if !strings.Contains(string(raw), "flavor-test-free") {
		t.Fatalf("文件内容缺模型: %s", raw)
	}
	_ = kit.WriteFileAtomicDefault // 引用避免在某些构建配置下的 unused 报错
}
