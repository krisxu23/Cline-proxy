package app

import (
	"os"
	"strings"
	"testing"
)

// claude_thinking_blocks.go 的**源码层接线锁**。
//
// ★ 断言要点（沿用 NEG-CACHE-A 的教训）: 不仅要断言"调用落在 anthropic 分支内"，
//   还必须断言**调用次数 == 1** —— 只断言"第一次出现的位置"会漏掉"调用被同时
//   塞进两个分支"这类作用域错误。

func TestThinking接线_源码层锁死(t *testing.T) {
	b, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读取 providers_chat.go 失败: %v", err)
	}
	s := string(b)

	callMarker := "applyClaudeThinkingBlocks("

	// --- 1) 全文件唯一调用点 ---
	if n := strings.Count(s, callMarker); n != 1 {
		t.Fatalf("providers_chat.go 内 %s 应恰好出现 1 次, 实际 %d 次", callMarker, n)
	}
	callIdx := strings.Index(s, callMarker)

	// --- 2) 必须落在 anthropic 分支内 ---
	// 分支边界: `if cfg.APIType == "anthropic" {` 与紧随其后的
	// `} else if msgs, ok := params["messages"]; ok {`。
	anthropicIf := strings.Index(s, `if cfg.APIType == "anthropic" {`)
	if anthropicIf < 0 {
		t.Fatalf("未找到 anthropic 分支起点")
	}
	elseIf := strings.Index(s, `} else if msgs, ok := params["messages"]; ok {`)
	if elseIf < 0 {
		t.Fatalf("未找到 else-if 边界")
	}
	if callIdx < anthropicIf || callIdx > elseIf {
		t.Fatalf("调用点不在 anthropic 分支开区间内 (call=%d, if=%d, else=%d)",
			callIdx, anthropicIf, elseIf)
	}

	// --- 3) 必须晚于 reanchorClaudePromptCache ---
	// 参考实现的 Pass 2 反向循环里, "打 cache_control" 与 "处理 thinking"
	// 是同一次遍历；我方拆成两步后，thinking 必须排在 cache 重锚之后
	// （重锚会把 messages 的 content 规整成数组，thinking 段依赖该形态）。
	cacheIdx := strings.Index(s, "reanchorClaudePromptCache(params,")
	if cacheIdx < 0 {
		t.Fatalf("未找到 reanchorClaudePromptCache 调用")
	}
	if callIdx < cacheIdx {
		t.Fatalf("thinking 处理必须晚于 cache 重锚")
	}

	// --- 4) 必须接受 mode-selection 参数, 不得写死 true ---
	// ★ 这是"照抄必须连作用域一起抄"的第二次教训: 参考实现的
	//   modelTargetsClaude 取自 per-model 注册表(providerModels.ts:172)，
	//   写死 true 会让非 Anthropic 上游错误地拿到 redacted_thinking{data}，
	//   而那正是参考实现注释里点名的 400 "Invalid signature in thinking block"。
	seg := s[callIdx : callIdx+600]
	if !strings.Contains(seg, "supportsPromptCachingForProvider(") {
		t.Fatalf("调用点必须按 provider 能力决定 targetsClaude，不得写死 true；片段:\n%s", seg)
	}
	if strings.Contains(seg, "true, nil, nil") && !strings.Contains(seg, "ForProvider") {
		t.Fatalf("疑似写死 targetsClaude=true")
	}

	// --- 5) 实现文件里三个分支常量必须在 ---
	impl, err := os.ReadFile("claude_thinking_blocks.go")
	if err != nil {
		t.Fatalf("读取 claude_thinking_blocks.go 失败: %v", err)
	}
	is := string(impl)
	for _, marker := range []string{
		"isKimiCodingProvider",
		"supportsPromptCachingForProvider",
		"supportsRedactedThinking",
		"defaultThinkingClaudeSignature",
		"nonAnthropicThinkingPlaceholder",
	} {
		if !strings.Contains(is, marker) {
			t.Fatalf("实现文件缺少 %s", marker)
		}
	}
}
