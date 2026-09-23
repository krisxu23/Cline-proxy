package app

import (
	"strings"
	"testing"
)

// 本文件是 tool_calling_required_check.go 的权威行为规格，
// 逐条对应 OmniRoute open-sse/handlers/chatCore/toolCallingRequiredCheck.ts。
//
// 该模块防护的正是用户报的"任务无声中断 / 模型假装执行了动作"故障:
// 目标模型不支持工具调用时, 若静默剥掉 tools 并扁平化历史, 会返回一个
// HTTP 200 却永远无法真正调用工具的响应。直连请求没有备选目标, 必须显式报错。

func TestToolCallingRequired_组合请求不拦截(t *testing.T) {
	// :25 `if (isCombo) return { blocked: false };`
	body := map[string]any{"tools": []any{map[string]any{"type": "function"}}}
	got := checkToolCallingRequiredButUnsupported(body, []string{"tools"}, true, "m")
	if got.blocked {
		t.Fatal("组合请求不拦截 (还有别的目标可换)")
	}
}

func TestToolCallingRequired_不支持清单里没有tools不拦截(t *testing.T) {
	// :26 `if (!unsupported.includes("tools")) return { blocked: false };`
	body := map[string]any{"tools": []any{map[string]any{"type": "function"}}}
	got := checkToolCallingRequiredButUnsupported(body, []string{"temperature", "top_p"}, false, "m")
	if got.blocked {
		t.Fatal("不支持清单不含 tools 时不拦截")
	}
}

func TestToolCallingRequired_缺少tools键不拦截(t *testing.T) {
	// :27 `if (!Array.isArray(body.tools) || body.tools.length === 0) return ...`
	got := checkToolCallingRequiredButUnsupported(map[string]any{}, []string{"tools"}, false, "m")
	if got.blocked {
		t.Fatal("没有 tools 键时不拦截")
	}
}

func TestToolCallingRequired_tools为空数组不拦截(t *testing.T) {
	// :27 后半段 `body.tools.length === 0`
	got := checkToolCallingRequiredButUnsupported(
		map[string]any{"tools": []any{}}, []string{"tools"}, false, "m")
	if got.blocked {
		t.Fatal("tools 为空数组时不拦截")
	}
}

func TestToolCallingRequired_tools为非数组不拦截(t *testing.T) {
	// :27 `!Array.isArray(body.tools)` —— 畸形客户端传字符串/对象时不拦截
	for _, v := range []any{"notarray", map[string]any{"a": 1}, float64(1), nil} {
		got := checkToolCallingRequiredButUnsupported(
			map[string]any{"tools": v}, []string{"tools"}, false, "m")
		if got.blocked {
			t.Fatalf("tools=%#v 非数组时不拦截", v)
		}
	}
}

func TestToolCallingRequired_直连加不支持加非空tools则拦截(t *testing.T) {
	// :29-32 四条都成立 → blocked + 带模型名的明确报错
	body := map[string]any{"tools": []any{map[string]any{"type": "function"}}}
	got := checkToolCallingRequiredButUnsupported(body, []string{"tools"}, false, "behemoth-x-123b")
	if !got.blocked {
		t.Fatal("应拦截")
	}
	// 报错文案逐字对照 :31
	want := `Model "behemoth-x-123b" does not support tool calling. ` +
		`Remove "tools" from the request or choose a different model.`
	if got.message != want {
		t.Fatalf("message = %q\n期望     = %q", got.message, want)
	}
}

func TestToolCallingRequired_报错文案包含模型名(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"n": 1}}}
	got := checkToolCallingRequiredButUnsupported(body, []string{"tools"}, false, "some/model-v2")
	if !strings.Contains(got.message, "some/model-v2") {
		t.Fatalf("message 必须含模型名, 实际 %q", got.message)
	}
}

// --- responses_item_id ---

func TestResponsesItemId_仅字符串合法(t *testing.T) {
	// responsesItemId.ts:5-7 `return typeof id === "string";`
	if !isValidResponsesItemId("item_123") {
		t.Fatal("字符串应合法")
	}
	if !isValidResponsesItemId("") {
		t.Fatal("空串也是 string, 应合法")
	}
	// 非字符串一律非法 —— **不做隐式转换**
	for _, v := range []any{nil, float64(1), true, map[string]any{}, []any{}} {
		if isValidResponsesItemId(v) {
			t.Fatalf("值 %#v 应判非法 (typeof !== string)", v)
		}
	}
}
