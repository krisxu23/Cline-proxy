package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// extractStringContent 的接入级测试 —— 锁定 billing header 剥离真的接上了。
//
// ★ 权威语义来自 .negbak/probe_billingheader.mjs（B1–B12）。

func TestExtractStringContent_字符串形态剥billing头(t *testing.T) {
	raw := json.RawMessage(`"x-anthropic-billing-header: abc123\nYou are a helpful assistant."`)
	got := extractStringContent(raw)
	if got != "You are a helpful assistant." {
		t.Fatalf("billing header 应被剥离, got %q", got)
	}
}

func TestExtractStringContent_块数组形态逐块剥(t *testing.T) {
	// 参考实现在块数组分支里对每块调 stripAnthropicBillingHeader
	// （claude-to-openai.ts:155）。
	raw := json.RawMessage(`[
		{"type":"text","text":"x-anthropic-billing-header: abc\nBlock one"},
		{"type":"text","text":"Block two"},
		{"type":"text","text":"x-anthropic-billing-header: def\nBlock three"}
	]`)
	got := extractStringContent(raw)
	want := "Block one\nBlock two\nBlock three"
	if got != want {
		t.Fatalf("每块都应剥离: got %q, want %q", got, want)
	}
}

func TestExtractStringContent_无头时原样(t *testing.T) {
	raw := json.RawMessage(`"You are a helpful assistant."`)
	if got := extractStringContent(raw); got != "You are a helpful assistant." {
		t.Fatalf("无 billing 头应原样, got %q", got)
	}
	raw2 := json.RawMessage(`[{"type":"text","text":"plain"},{"type":"text","text":"text"}]`)
	if got := extractStringContent(raw2); got != "plain\ntext" {
		t.Fatalf("无 billing 头应原样拼接, got %q", got)
	}
}

func TestExtractStringContent_非text块不影响(t *testing.T) {
	// image 等非 text 块本就被过滤（既有行为），此处只确认剥离不改变它。
	raw := json.RawMessage(`[
		{"type":"image","source":{"type":"base64","data":"x"}},
		{"type":"text","text":"x-anthropic-billing-header: abc\nVisible"}
	]`)
	if got := extractStringContent(raw); got != "Visible" {
		t.Fatalf("应只保留 text 块并剥离头部, got %q", got)
	}
}

func TestExtractStringContent_空与非法原样(t *testing.T) {
	if got := extractStringContent(nil); got != "" {
		t.Fatalf("nil 应返回空串, got %q", got)
	}
	if got := extractStringContent(json.RawMessage(``)); got != "" {
		t.Fatalf("空 raw 应返回空串, got %q", got)
	}
	// 非法 JSON 走两个 Unmarshal 都失败 -> 空串（既有行为）
	if got := extractStringContent(json.RawMessage(`{invalid`)); got != "" {
		t.Fatalf("非法 JSON 应返回空串, got %q", got)
	}
}

// TestExtractStringContent_源码层锁死 锁住剥离调用点在函数内，防止被"顺手删掉"。
func TestExtractStringContent_源码层锁死(t *testing.T) {
	src, err := os.ReadFile("anthropic.go")
	if err != nil {
		t.Fatalf("读 anthropic.go 失败: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, "stripAnthropicBillingHeader") {
		t.Fatalf("接线缺失: extractStringContent 未调用 stripAnthropicBillingHeader")
	}
	// 字符串分支与块数组分支**都要**剥 —— 参考实现两处都调
	// （claude-to-openai.ts:145 与 :155）。
	if n := strings.Count(s, "stripAnthropicBillingHeader("); n < 2 {
		t.Fatalf("接线不完整: 应至少 2 处调用(字符串分支 + 块数组分支), got %d", n)
	}
	// 必须落在 extractStringContent 函数体内
	fnStart := strings.Index(s, "func extractStringContent(")
	if fnStart < 0 {
		t.Fatalf("结构缺失: 未找到 extractStringContent")
	}
	call := strings.Index(s, "stripAnthropicBillingHeader(")
	if call < fnStart {
		t.Fatalf("接线作用域错误: 剥离调用(%d)不在 extractStringContent(%d) 之后", call, fnStart)
	}
}
