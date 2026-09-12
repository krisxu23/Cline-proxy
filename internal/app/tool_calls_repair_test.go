package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 客户端(Cline 类 agent 工具)对 tool_calls 有硬校验: id 与 function.name
// 缺一即整体报 "Model provider returned tool_calls without a complete id
// and function name"。部分 free 模型恰好只回 name 不回 id。
func TestRepairToolCalls(t *testing.T) {
	in := []any{
		// 只缺 id: 补生成即可用
		map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "read_file", "arguments": `{"path":"a.go"}`},
		},
		// 连 name 都没有: 无法执行, 剔除
		map[string]any{
			"id":       "call_x",
			"type":     "function",
			"function": map[string]any{"arguments": "{}"},
		},
		// 缺 id 且缺 arguments: 补 id 并给默认空参数
		map[string]any{
			"function": map[string]any{"name": "list_dir"},
		},
	}
	out, dropped := repairToolCalls(in)
	if dropped != 1 {
		t.Fatalf("应剔除 1 个无名条目, 实际 %d", dropped)
	}
	if len(out) != 2 {
		t.Fatalf("应保留 2 个可用条目, 实际 %d", len(out))
	}
	first := out[0].(map[string]any)
	if id, _ := first["id"].(string); !strings.HasPrefix(id, "call_") {
		t.Fatalf("缺失的 id 应补成 call_ 前缀, 得到 %q", id)
	}
	third := out[1].(map[string]any)
	if id, _ := third["id"].(string); !strings.HasPrefix(id, "call_") {
		t.Fatalf("缺失的 id 应补成 call_ 前缀, 得到 %q", id)
	}
	fn, _ := third["function"].(map[string]any)
	if args, _ := fn["arguments"].(string); args != "{}" {
		t.Fatalf("缺失的 arguments 应补默认 {}, 得到 %q", args)
	}
	if typ, _ := third["type"].(string); typ != "function" {
		t.Fatalf("缺失的 type 应补 function, 得到 %q", typ)
	}
}

// 流式 delta 只补"起始块"缺的 id; 续流块(只带 arguments 分片)保持原样,
// 客户端按 index 累积分片, 动了续流块反而会把流弄坏。
func TestRepairToolCallDeltas(t *testing.T) {
	tcs := []any{
		// 起始块缺 id: 要补
		map[string]any{"index": 0.0, "type": "function", "function": map[string]any{"name": "read_file", "arguments": ""}},
		// 续流块: 无 id 无 name 只有参数分片, 不能动
		map[string]any{"index": 0.0, "function": map[string]any{"arguments": `"a.go`}},
	}
	repairToolCallDeltas(tcs)
	first := tcs[0].(map[string]any)
	if id, _ := first["id"].(string); !strings.HasPrefix(id, "call_") {
		t.Fatalf("起始块缺 id 应补, 得到 %q", id)
	}
	second := tcs[1].(map[string]any)
	if _, has := second["id"]; has {
		t.Fatal("续流块不应被补 id")
	}
}

// 修补归入 normalizeMessage: 非流式响应出口处完成修补。
func TestNormalizeMessageRepairsToolCalls(t *testing.T) {
	msg := map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "read_file", "arguments": "{}"}},
		},
	}
	out := normalizeMessage(msg)
	tcs, _ := out["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("应保留 1 条 tool_call, 得到 %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if id, _ := tc["id"].(string); !strings.HasPrefix(id, "call_") {
		t.Fatalf("出口处应补齐 id, 得到 %q", id)
	}
}

// 分类: 全部缺 name 才算"不可用"; 缺 id 的可修补, 不算。
func TestChainBodyOnlyBrokenToolCalls(t *testing.T) {
	if chainBodyOnlyBrokenToolCalls([]byte(`{"choices":[{"message":{"content":"hi"}}]}`)) {
		t.Fatal("没有 tool_calls 不应命中")
	}
	if chainBodyOnlyBrokenToolCalls([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`)) {
		t.Fatal("完整的 tool_calls 不应命中")
	}
	if chainBodyOnlyBrokenToolCalls([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}}]}`)) {
		t.Fatal("只缺 id 的可修补, 不应命中")
	}
	if !chainBodyOnlyBrokenToolCalls([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"arguments":"{}"}}]}}]}`)) {
		t.Fatal("全部缺 name 应命中")
	}
}

// 端到端: 上游只回 name 不回 id, 客户端拿到的必须是补全后的调用。
func TestChainRepairsMissingToolCallID(t *testing.T) {
	base, _ := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-tc-noid"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"read_file"`) {
		t.Fatalf("tool_call 的 name 必须保留: %s", body)
	}
	if !strings.Contains(body, `"call_`) {
		t.Fatalf("缺失的 id 必须在出口补齐: %s", body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID == "" || tc.Function.Name == "" {
		t.Fatalf("id 与 name 必须齐全: %+v", tc)
	}
}

// 端到端: 上游回了"无法执行"的 tool_calls(全缺 name)且没有正文,
// 候选链必须换下一站, 而不是把残缺响应丢给客户端。
func TestChainFailsOverOnBrokenToolCalls(t *testing.T) {
	base, calls := chainTestServer(t)
	chainTestSetup(t, base, map[string][]string{"r": {"p1:m-tc-nameless", "p2:ok"}})

	rec := runChain(t, "r", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("broken tool_calls must fail over, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls("m-tc-nameless") != 1 {
		t.Fatalf("broken hop must be tried once, got %d", calls("m-tc-nameless"))
	}
	if why := candidateSkipReason("p1", "m-tc-nameless"); why == "" {
		t.Fatal("broken hop must be cooled so later requests skip it")
	}
}
