package app

import (
	"os"
	"strings"
	"testing"
)

// 源码层接线锁：复刻测试证明不了"接线在"，必须用 os.ReadFile 直接检查
// 真实调用点。参见 memory：接线锁断言必须用「真实调用形态」定位，
// 裸 token 会先命中注释里的同一串字面量导致假红。
//
// 本文件锁死 claude_toolschema.go 三个函数在 anthropic.go 里的接入位置。

func readSourceFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("读不到 %s: %v", rel, err)
	}
	return string(b)
}

func TestToolSchema接线_anthropicReq结构有双字段(t *testing.T) {
	src := readSourceFile(t, "anthropic.go")

	// 参考实现靠 `body.thinking?.type` / `body.output_config?.effort` 读这两个字段，
	// 我方靠 anthropicReq 的 JSON tag 解出来再喂 openAIReasoningEffort。
	// 缺任一字段 = 该方言永远读不到 = 推理强度档永远落默认。
	for _, field := range []struct{ decl, tag string }{
		{"Thinking", `json:"thinking,omitempty"`},
		{"OutputConfig", `json:"output_config,omitempty"`},
	} {
		if !strings.Contains(src, field.decl) {
			t.Fatalf("anthropicReq 缺字段 %s（参考实现 :257/:254 需要它）", field.decl)
		}
		if !strings.Contains(src, field.tag) {
			t.Fatalf("anthropicReq 的 %s 缺 JSON tag %s", field.decl, field.tag)
		}
	}
}

func TestToolSchema接线_normalizeToolSchema包在sanitize外(t *testing.T) {
	src := readSourceFile(t, "anthropic.go")

	// 照抄 claude-to-openai.ts:232 `parameters: normalizeToolSchema(record.input_schema)`。
	// 参考实现是两步：executor sanitize（剥非法构造）→ translator normalize（补 properties）。
	// 断言**完整调用形态**（带参数与赋值），避免命中注释里的裸 token。
	want := "schema := normalizeToolSchema(sanitizeClaudeToolSchema(tMap[\"input_schema\"]))"
	n := strings.Count(src, want)
	if n != 1 {
		t.Fatalf("anthropicToolsToOpenAI 里 %q 出现 %d 次，期望恰好 1 次\n来源: claude-to-openai.ts:232", want, n)
	}

	// 顺序约束：normalize 必须在 sanitize 之外层（先剥再补）。
	inner := strings.Index(src, "sanitizeClaudeToolSchema(tMap[")
	outer := strings.Index(src, "normalizeToolSchema(sanitizeClaudeToolSchema(tMap[")
	if outer < 0 || inner < 0 {
		t.Fatalf("找不到调用点: outer=%d inner=%d", outer, inner)
	}
	// outer 位置必然 <= inner 位置（外层括号的起始字符在前）
	if outer > inner {
		t.Fatalf("normalizeToolSchema(%d) 应在 sanitizeClaudeToolSchema(%d) 之外层", outer, inner)
	}

	// 裸调用 sanitizeClaudeToolSchema(tMap["input_schema"]) 应当只剩 0 次 ——
	// 证明旧写法已被替换，没有残留死分支。
	stale := strings.Count(src, "schema := sanitizeClaudeToolSchema(tMap[\"input_schema\"])")
	if stale != 0 {
		t.Fatalf("anthropic.go 仍有 %d 处未包 normalizeToolSchema 的旧调用形态", stale)
	}
}

func TestToolSchema接线_reasoningEffort写入位置(t *testing.T) {
	src := readSourceFile(t, "anthropic.go")

	// 照抄 claude-to-openai.ts:254-270。断言完整调用形态。
	call := "if e := openAIReasoningEffort(body); e != \"\""
	if n := strings.Count(src, call); n != 1 {
		t.Fatalf("%q 出现 %d 次，期望恰好 1 次\n来源: claude-to-openai.ts:254-256", call, n)
	}

	set := `openAI["reasoning_effort"] = e`
	if n := strings.Count(src, set); n != 1 {
		t.Fatalf("%q 出现 %d 次，期望恰好 1 次\n来源: claude-to-openai.ts:256/262/264/266/268", set, n)
	}

	// 顺序约束：reasoning_effort 在 tool_choice 之后（参考实现 :243-249 → :251-270）。
	tcIdx := strings.Index(src, `openAI["tool_choice"] = req.ToolChoice`)
	effIdx := strings.Index(src, call)
	if tcIdx < 0 || effIdx < 0 {
		t.Fatalf("找不到调用点: tool_choice=%d effort=%d", tcIdx, effIdx)
	}
	if effIdx <= tcIdx {
		t.Fatalf("reasoning_effort 写入(%d) 必须在 tool_choice(%d) 之后，与参考实现 :243→:251 顺序一致", effIdx, tcIdx)
	}

	// 实现文件本身要三个函数都在。
	impl := readSourceFile(t, "claude_toolschema.go")
	for _, tok := range []string{
		"func normalizeToolSchema(schema any) any",
		"func normalizeOpenAIReasoningEffort(effort any) string",
		"func openAIReasoningEffort(body map[string]any) string",
	} {
		if !strings.Contains(impl, tok) {
			t.Fatalf("claude_toolschema.go 缺 %s", tok)
		}
	}

	// 分档阈值锁：与 thinkingBudget.ts:316-324 反向映射逐值一致。
	for _, tok := range []string{
		`case budget <= 0:`,
		`case budget <= 1024:`,
		`case budget <= 10240:`,
		`case budget < 131072:`,
		`return "low"`,
		`return "medium"`,
		`return "high"`,
		`return "xhigh"`,
	} {
		if !strings.Contains(impl, tok) {
			t.Fatalf("claude_toolschema.go 缺分档分支 %q", tok)
		}
	}
}
