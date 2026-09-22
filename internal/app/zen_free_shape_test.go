package app

import (
	"testing"
)

// zenFreeShapeRequired 的判定: 免费层走任何端点都要求整形, 付费模型永远不要求。
func TestZenFreeShapeRequired(t *testing.T) {
	cases := []struct {
		name     string
		endpoint zenEndpointKind
		model    string
		want     bool
	}{
		{"suffix-free-chat", zenEndpointChat, "mimo-v2.5-free", true},
		{"suffix-free-responses", zenEndpointResponses, "muse-spark-1.3-contributor-free", true},
		{"messages-suffix-free", zenEndpointMessages, "messages-only-free", true},
		{"empty-model-no-throw", zenEndpointChat, "", false},
		{"paid-model-not-shaped", zenEndpointChat, "claude-sonnet-5", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := zenFreeShapeRequired(c.endpoint, c.model)
			if got != c.want {
				t.Fatalf("endpoint=%v model=%q got=%v want=%v", c.endpoint, c.model, got, c.want)
			}
		})
	}
}

// zenApplyFreeShape 在 chat 端点上强制 stream + 注入 agent tools。
func TestZenApplyFreeShapeChatForcesStreamAndTools(t *testing.T) {
	body := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, forced := zenApplyFreeShape(zenEndpointChat, body)
	if !forced {
		t.Fatal("非流式原始请求必须报告 forced=true")
	}
	if s, _ := out["stream"].(bool); !s {
		t.Fatal("stream 必须被强制为 true")
	}
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != len(zenAgentCoreTools) {
		t.Fatalf("应注入 %d 个 agent tools, got %v", len(zenAgentCoreTools), out["tools"])
	}
	// chat 形状: function 嵌套
	first, _ := tools[0].(map[string]any)
	fn, ok := first["function"].(map[string]any)
	if !ok {
		t.Fatalf("chat 端点 tools[0] 必须是 function 嵌套形状, got %v", first)
	}
	if n, _ := fn["name"].(string); n != "bash" {
		t.Fatalf("tools[0].function.name 应为 bash, got %q", n)
	}
	// chat 端点必须设置 stream_options.include_usage
	opts, _ := out["stream_options"].(map[string]any)
	if inc, _ := opts["include_usage"].(bool); !inc {
		t.Fatal("chat 端点必须设置 stream_options.include_usage=true")
	}
}

// zenApplyFreeShape 保留客户端已有的 tool 定义, 只补缺失的。
func TestZenApplyFreeShapePreservesClientTools(t *testing.T) {
	clientTools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "custom_tool"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "bash"}}, // 已存在, 不该重复
	}
	body := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    clientTools,
	}
	out, _ := zenApplyFreeShape(zenEndpointChat, body)
	tools, _ := out["tools"].([]any)
	// 客户端 2 个 + 补 4 个 (edit/glob/grep/read)
	if len(tools) != 6 {
		t.Fatalf("应保留 2 个客户端工具 + 追加 4 个 agent 工具, got %d", len(tools))
	}
	// 客户端原定义应保留在开头
	first, _ := tools[0].(map[string]any)
	fn, _ := first["function"].(map[string]any)
	if n, _ := fn["name"].(string); n != "custom_tool" {
		t.Fatalf("客户端工具应保留在原位置, got %v", first)
	}
}

// 客户端原始请求 stream=true 时, 不应报告 forced=true(调用方直接透传 SSE)。
func TestZenApplyFreeShapeDoesNotForceAlreadyStreaming(t *testing.T) {
	body := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	_, forced := zenApplyFreeShape(zenEndpointChat, body)
	if forced {
		t.Fatal("客户端已请求 stream=true, 不需要报告 forced")
	}
}

// 付费模型 zenApplyFreeShape 应完全透传, 不动 body 内容。
func TestZenApplyFreeShapeSkipsPaidModels(t *testing.T) {
	body := map[string]any{
		"model":    "claude-sonnet-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	out, forced := zenApplyFreeShape(zenEndpointChat, body)
	if forced {
		t.Fatal("付费模型不应触发整形")
	}
	if out["stream"] != false {
		t.Fatal("付费模型的 stream 必须保持原值")
	}
	if _, has := out["tools"]; has {
		t.Fatal("付费模型不应被注入 tools")
	}
	if _, has := out["stream_options"]; has {
		t.Fatal("付费模型不应被注入 stream_options")
	}
}

// responses 端点使用扁平 tool 形状 (无 function 嵌套)。
func TestZenApplyFreeShapeResponsesShape(t *testing.T) {
	body := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, _ := zenApplyFreeShape(zenEndpointResponses, body)
	tools, _ := out["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("responses 端点也必须注入 agent tools")
	}
	first, _ := tools[0].(map[string]any)
	// 扁平形状: name 直接在顶层, 而不是嵌在 function 里
	if n, _ := first["name"].(string); n != "bash" {
		t.Fatalf("responses 端点 tools[0].name 应为 bash(扁平形状), got %v", first)
	}
	if _, has := first["function"]; has {
		t.Fatalf("responses 端点 tools[0] 不应带 function 嵌套, got %v", first)
	}
	// responses 端点不应加 stream_options(上游按 SSE 直接返回, 无需 include_usage)
	if _, has := out["stream_options"]; has {
		t.Fatal("responses 端点不应注入 stream_options")
	}
}

// messages 端点使用 claude 形状 (input_schema)。
func TestZenApplyFreeShapeMessagesShape(t *testing.T) {
	// 用 -free 后缀走 suffix 判定路径, 不依赖 zen 模型目录。
	body := map[string]any{
		"model": "messages-only-free",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, _ := zenApplyFreeShape(zenEndpointMessages, body)
	tools, _ := out["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("messages 端点也必须注入 agent tools")
	}
	first, _ := tools[0].(map[string]any)
	if n, _ := first["name"].(string); n != "bash" {
		t.Fatalf("messages 端点 tools[0].name 应为 bash, got %v", first)
	}
	if _, has := first["input_schema"]; !has {
		t.Fatalf("messages 端点 tools[0] 必须是 claude 形状(input_schema), got %v", first)
	}
	if _, has := first["function"]; has {
		t.Fatalf("messages 端点 tools[0] 不应带 function 嵌套, got %v", first)
	}
	if _, has := out["stream_options"]; has {
		t.Fatal("messages 端点不应注入 stream_options")
	}
}
