package app

import (
	"reflect"
	"testing"
)

// 本测试逐条对应用户要求照抄的 OmniRoute 参考用例:
//
//	tests/unit/reasoningContentInjector.test.ts          → TestIsThinkingMessageModel / TestInjectReasoningContent...
//	tests/unit/reasoning-fields-placeholder-strip.test.ts → TestCopyOpenAICompatibleReasoningFields
//	tests/unit/reasoning-placeholder-strip.test.ts        → TestStripInternalReasoningPlaceholder
//	tests/unit/opencodeReasoningSanitizer.test.ts         → TestIsOpencodeGoProvider / TestStripBooleanReasoning
//
// 用例设计照搬原文件, 不自行发明断言。

// ─────────────── reasoningContentInjector.ts 对应用例 ───────────────

func TestIsThinkingMessageModel(t *testing.T) {
	// 原: "recognizes xiaomi-tokenplan/mimo-v2.5-pro as a thinking-mode model"
	if !isThinkingMessageModel("xiaomi-tokenplan/mimo-v2.5-pro") {
		t.Fatal("xiaomi-tokenplan/mimo-v2.5-pro 应识别为 thinking 模型")
	}
	// 原: "recognizes bare mimo model ids as thinking-mode models"
	if !isThinkingMessageModel("mimo-v2.5-pro") {
		t.Fatal("mimo-v2.5-pro 应识别为 thinking 模型")
	}
	// 原: "still recognizes the existing thinking-mode families (deepseek/kimi/k2/minimax)"
	for _, model := range []string{"deepseek-v4-flash", "kimi-k2", "minimax-m2"} {
		if !isThinkingMessageModel(model) {
			t.Fatalf("%s 应识别为 thinking 模型", model)
		}
	}
	// 原: "does not flag unrelated model ids"
	if isThinkingMessageModel("gpt-4o") {
		t.Fatal("gpt-4o 不应被识别为 thinking 模型")
	}
}

func TestIsThinkingMessageModel_边界(t *testing.T) {
	// 空值与错误类型: 原实现 `if (!model || typeof model !== "string") return false`
	if isThinkingMessageModel("") {
		t.Fatal("空模型 ID 应返回 false")
	}
	if isThinkingMessageModel(nil) {
		t.Fatal("nil 模型 ID 应返回 false")
	}
	if isThinkingMessageModel(42) {
		t.Fatal("非字符串模型 ID 应返回 false")
	}
	// 大小写不敏感: 原实现所有正则带 /i
	if !isThinkingMessageModel("DeepSeek-V4-Flash") {
		t.Fatal("模型 ID 匹配应大小写不敏感")
	}
}

func TestInjectReasoningContentForThinkingModel(t *testing.T) {
	// 原: "injects a reasoning_content placeholder for assistant messages when routed to mimo"
	body := map[string]any{
		"model": "xiaomi-tokenplan/mimo-v2.5-pro",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
	}
	if !isThinkingMessageModel(body["model"]) {
		t.Fatal("前置条件失败: 该模型应识别为 thinking 模型")
	}

	injected, ok := injectReasoningContentForThinkingModel(body).(map[string]any)
	if !ok {
		t.Fatal("注入结果应为 map")
	}
	messages := injected["messages"].([]any)
	assistantMsg := messages[1].(map[string]any)
	if got := assistantMsg["reasoning_content"]; got != " " {
		t.Fatalf("assistant 消息 reasoning_content 应为单个空格占位符, 实得 %#v", got)
	}
}

// TestInjectReasoningContentForThinkingModel_多条历史全部注入 覆盖真实网关场景:
// agent 多轮工具调用的历史里有 N 条 assistant 消息, 每一条都必须带上
// reasoning_content, 否则上游报 400 "reasoning_content must be passed back"。
func TestInjectReasoningContentForThinkingModel_多条历史全部注入(t *testing.T) {
	body := map[string]any{
		"model": "mimo-v2.5-pro",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "u1"},
			map[string]any{"role": "assistant", "content": "a1", "tool_calls": []any{map[string]any{"id": "t1"}}},
			map[string]any{"role": "tool", "content": "r1", "tool_call_id": "t1"},
			map[string]any{"role": "user", "content": "u2"},
			map[string]any{"role": "assistant", "content": "a2", "tool_calls": []any{map[string]any{"id": "t2"}}},
			map[string]any{"role": "tool", "content": "r2", "tool_call_id": "t2"},
			map[string]any{"role": "assistant", "content": "a3"},
		},
	}
	injected := injectReasoningContentForThinkingModel(body).(map[string]any)
	messages := injected["messages"].([]any)

	// 三条 assistant 消息必须全部注入, 且位置与条数不变
	if len(messages) != len(body["messages"].([]any)) {
		t.Fatalf("消息条数不得变化, 实得 %d", len(messages))
	}
	assistantIndexes := []int{}
	for i, raw := range messages {
		msg := raw.(map[string]any)
		if msg["role"] == "assistant" {
			assistantIndexes = append(assistantIndexes, i)
			if got := msg["reasoning_content"]; got != " " {
				t.Fatalf("第 %d 条 assistant 消息应注入占位符, 实得 %#v", i, got)
			}
		}
	}
	if len(assistantIndexes) != 3 {
		t.Fatalf("应识别到 3 条 assistant 消息, 实得 %d", len(assistantIndexes))
	}

	// 非 assistant 消息不得被写入 reasoning_content
	for i, raw := range messages {
		msg := raw.(map[string]any)
		if msg["role"] != "assistant" {
			if _, exists := msg["reasoning_content"]; exists {
				t.Fatalf("第 %d 条非 assistant 消息不应被注入 reasoning_content", i)
			}
		}
	}

	// assistant 消息上的其余字段(如 tool_calls)必须保留
	first := messages[assistantIndexes[0]].(map[string]any)
	if first["content"] != "a1" {
		t.Fatalf("assistant content 不应被改动, 实得 %#v", first["content"])
	}
	if _, exists := first["tool_calls"]; !exists {
		t.Fatal("assistant 的 tool_calls 字段必须保留")
	}
}

// TestInjectReasoningContentForThinkingModel_已带推理不覆盖 对照上一条:
// 历史里部分消息已有真实 reasoning_content 时, 只补缺失的那部分。
func TestInjectReasoningContentForThinkingModel_已带推理不覆盖(t *testing.T) {
	body := map[string]any{
		"model": "mimo-v2.5-pro",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "a1", "reasoning_content": "真实推理一"},
			map[string]any{"role": "user", "content": "u2"},
			map[string]any{"role": "assistant", "content": "a2"},
		},
	}
	injected := injectReasoningContentForThinkingModel(body).(map[string]any)
	messages := injected["messages"].([]any)

	if got := messages[0].(map[string]any)["reasoning_content"]; got != "真实推理一" {
		t.Fatalf("已有真实推理不得被覆盖, 实得 %#v", got)
	}
	if got := messages[2].(map[string]any)["reasoning_content"]; got != " " {
		t.Fatalf("缺失的那条应被补占位符, 实得 %#v", got)
	}
}

func TestInjectReasoningContentForThinkingModel_不改动场景(t *testing.T) { // 原实现: "Returns the original object if no mutation was needed"。
	// 已带非空 reasoning_content 的 assistant 消息不应被覆盖。
	body := map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "hello", "reasoning_content": "real reasoning"},
		},
	}
	if got := injectReasoningContentForThinkingModel(body); !reflect.DeepEqual(got, body) {
		t.Fatal("无需改动时应返回原对象(含原始 reasoning_content)")
	}

	// 只有 user 消息时同样无需改动
	userOnly := map[string]any{
		"model":    "deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if got := injectReasoningContentForThinkingModel(userOnly); !reflect.DeepEqual(got, userOnly) {
		t.Fatal("无 assistant 消息时应返回原对象")
	}
}

func TestInjectReasoningContentForThinkingModel_纯空白视为缺失(t *testing.T) {
	// 原实现 hasNonEmptyReasoningContent 先 Trim 再判长度,
	// 因此纯空白的 reasoning_content 会被当作"缺失"而重新注入。
	body := map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "x", "reasoning_content": "   "},
		},
	}
	injected := injectReasoningContentForThinkingModel(body).(map[string]any)
	got := injected["messages"].([]any)[0].(map[string]any)["reasoning_content"]
	if got != " " {
		t.Fatalf("纯空白 reasoning_content 应被替换为占位符, 实得 %#v", got)
	}
}

func TestInjectReasoningContentForThinkingModel_防御式处理(t *testing.T) {
	// 原实现: "No-op when the body shape is unexpected (defensive)"
	for _, bad := range []any{
		nil,
		"not an object",
		42,
		[]any{1, 2},
		map[string]any{"model": "deepseek-v4-flash"}, // 无 messages
		map[string]any{"messages": "not an array"},
	} {
		if got := injectReasoningContentForThinkingModel(bad); !reflect.DeepEqual(got, bad) {
			t.Fatalf("形状异常时应原样返回, 输入 %#v 实得 %#v", bad, got)
		}
	}
}

func TestInjectReasoningContentForThinkingModel_不修改原对象(t *testing.T) {
	// 原实现用 { ...message } 浅拷贝, 原消息不应被写入 reasoning_content
	original := map[string]any{"role": "assistant", "content": "hello"}
	body := map[string]any{
		"model":    "mimo-v2.5-pro",
		"messages": []any{original},
	}
	injectReasoningContentForThinkingModel(body)
	if _, exists := original["reasoning_content"]; exists {
		t.Fatal("原消息对象不应被就地修改")
	}
}

// ─────────────── reasoning-placeholder-strip.test.ts 对应用例 ───────────────

func TestStripInternalReasoningPlaceholder(t *testing.T) {
	// 原: "a plain word-token chunk with a leading space is NOT trimmed"
	if got := stripInternalReasoningPlaceholder(" en"); got != " en" {
		t.Fatalf("无占位符时不应 trim, 实得 %q", got)
	}
	if got := stripInternalReasoningPlaceholder(" riktig"); got != " riktig" {
		t.Fatalf("无占位符时不应 trim, 实得 %q", got)
	}
	// 原: "a plain word-token chunk with a trailing space is NOT trimmed"
	if got := stripInternalReasoningPlaceholder("Bilden "); got != "Bilden " {
		t.Fatalf("无占位符时不应 trim, 实得 %q", got)
	}

	// 原: "reassembling word-token chunks preserves inter-word spaces"
	chunks := []string{"Bilden", " är", " en", " riktig", " JPEG", " nu"}
	rebuilt := ""
	for _, c := range chunks {
		rebuilt += stripInternalReasoningPlaceholder(c)
	}
	if rebuilt != "Bilden är en riktig JPEG nu" {
		t.Fatalf("流式 delta 拼接后空格被吃掉, 实得 %q", rebuilt)
	}

	// 原: "a chunk that IS exactly the placeholder collapses to empty string"
	if got := stripInternalReasoningPlaceholder(nonAnthropicThinkingPlaceholder); got != "" {
		t.Fatalf("整块即占位符时应折叠为空, 实得 %q", got)
	}
	if got := stripInternalReasoningPlaceholder("  " + nonAnthropicThinkingPlaceholder + "  "); got != "" {
		t.Fatalf("占位符带空白时应折叠为空, 实得 %q", got)
	}

	// 原: "a chunk with the placeholder mixed into real text strips it"
	want := "foo  bar" // replaceAll 留下双空格, trim 只作用于首尾
	if got := stripInternalReasoningPlaceholder("foo " + nonAnthropicThinkingPlaceholder + " bar"); got != want {
		t.Fatalf("混合文本剥离结果不符, 期望 %q 实得 %q", want, got)
	}

	// 原: "standalone whitespace-only chunks pass through byte-for-byte"
	for _, chunk := range []string{" ", "\t", "\n", "\n\n", "\r\n"} {
		if got := stripInternalReasoningPlaceholder(chunk); got != chunk {
			t.Fatalf("纯空白块 %q 应逐字节保留, 实得 %q", chunk, got)
		}
	}

	// 原: "an empty string stays empty"
	if got := stripInternalReasoningPlaceholder(""); got != "" {
		t.Fatalf("空串应保持为空, 实得 %q", got)
	}
}

func TestIsInternalReasoningPlaceholder(t *testing.T) {
	if !isInternalReasoningPlaceholder(nonAnthropicThinkingPlaceholder) {
		t.Fatal("占位符本身应判真")
	}
	if !isInternalReasoningPlaceholder("  " + nonAnthropicThinkingPlaceholder + "  ") {
		t.Fatal("带首尾空白应判真(实现先 TrimSpace)")
	}
	if isInternalReasoningPlaceholder("real reasoning") {
		t.Fatal("真实推理不应判真")
	}
	if isInternalReasoningPlaceholder("") || isInternalReasoningPlaceholder(nil) || isInternalReasoningPlaceholder(1) {
		t.Fatal("空值/非字符串应判假")
	}
}

// ─────────────── reasoning-fields-placeholder-strip.test.ts 对应用例 ───────────────

// copyReasoning 对应原测试里的 copy() helper。
func copyReasoning(source map[string]any) map[string]any {
	target := map[string]any{}
	copyOpenAICompatibleReasoningFields(source, target)
	return target
}

func TestCopyOpenAICompatibleReasoningFields(t *testing.T) {
	// 原: "real reasoning_content is preserved verbatim"
	if got := copyReasoning(map[string]any{"reasoning_content": "Let me think carefully."})["reasoning_content"]; got != "Let me think carefully." {
		t.Fatalf("真实 reasoning_content 应原样保留, 实得 %#v", got)
	}

	// 原: 各字段"恰好等于占位符时被丢弃"(参数化覆盖 5 个字段)
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text", "thinking", "thought"} {
		target := copyReasoning(map[string]any{field: nonAnthropicThinkingPlaceholder})
		if _, exists := target[field]; exists {
			t.Fatalf("字段 %s 恰好为占位符时应被删除", field)
		}
	}

	// 原: "no mirrored reasoning_content is emitted when the only signal is the placeholder"
	target := copyReasoning(map[string]any{"reasoning_text": nonAnthropicThinkingPlaceholder})
	if _, exists := target["reasoning_content"]; exists {
		t.Fatal("唯一信号是占位符时不应镜像出 reasoning_content")
	}
	if _, exists := target["reasoning_text"]; exists {
		t.Fatal("唯一信号是占位符时 reasoning_text 应被删除")
	}

	// 原: "placeholder embedded in otherwise real reasoning_text is stripped in place"
	embedded := copyReasoning(map[string]any{
		"reasoning_text": "First thought. " + nonAnthropicThinkingPlaceholder + " Second thought.",
	})["reasoning_text"]
	if embedded != "First thought.  Second thought." {
		t.Fatalf("内嵌占位符应就地剥离, 实得 %#v", embedded)
	}

	// 原: "all-placeholder reasoning_details are dropped entirely"
	allPlaceholder := copyReasoning(map[string]any{
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.text", "text": nonAnthropicThinkingPlaceholder},
			map[string]any{"type": "thinking", "content": "  " + nonAnthropicThinkingPlaceholder + "  "},
		},
	})
	if _, exists := allPlaceholder["reasoning_details"]; exists {
		t.Fatal("全为占位符的 reasoning_details 应整体删除")
	}
	if _, exists := allPlaceholder["reasoning_content"]; exists {
		t.Fatal("全为占位符时不应镜像 reasoning_content")
	}

	// 原: "mixed reasoning_details keep real text and drop only placeholder items"
	mixed := copyReasoning(map[string]any{
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.text", "text": "real first step "},
			map[string]any{"type": "thinking", "content": nonAnthropicThinkingPlaceholder},
			map[string]any{"type": "reasoning.text", "text": "real second step"},
		},
	})["reasoning_details"]
	wantMixed := []any{
		map[string]any{"type": "reasoning.text", "text": "real first step "},
		map[string]any{"type": "reasoning.text", "text": "real second step"},
	}
	if !reflect.DeepEqual(mixed, wantMixed) {
		t.Fatalf("混合 reasoning_details 净化结果不符\n期望 %#v\n实得 %#v", wantMixed, mixed)
	}

	// 原: "placeholder inside a reasoning_details text item is stripped in place"
	inPlace := copyReasoning(map[string]any{
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.text", "text": "real " + nonAnthropicThinkingPlaceholder + " tail"},
		},
	})["reasoning_details"]
	wantInPlace := []any{map[string]any{"type": "reasoning.text", "text": "real  tail"}}
	if !reflect.DeepEqual(inPlace, wantInPlace) {
		t.Fatalf("details 内嵌占位符应就地剥离\n期望 %#v\n实得 %#v", wantInPlace, inPlace)
	}

	// 原: "real reasoning_details still mirror into reasoning_content for readable clients"
	mirrored := copyReasoning(map[string]any{
		"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "real reasoning here"}},
	})
	if got := mirrored["reasoning_content"]; got != "real reasoning here" {
		t.Fatalf("真实 reasoning_details 应镜像到 reasoning_content, 实得 %#v", got)
	}
	if !reflect.DeepEqual(mirrored["reasoning_details"], []any{map[string]any{"type": "reasoning.text", "text": "real reasoning here"}}) {
		t.Fatalf("真实 reasoning_details 应原样保留, 实得 %#v", mirrored["reasoning_details"])
	}

	// 原: "non-text reasoning_details (e.g. reasoning.encrypted) survive untouched"
	encrypted := copyReasoning(map[string]any{
		"reasoning_details": []any{map[string]any{"type": "reasoning.encrypted", "data": "sig"}},
	})["reasoning_details"]
	wantEncrypted := []any{map[string]any{"type": "reasoning.encrypted", "data": "sig"}}
	if !reflect.DeepEqual(encrypted, wantEncrypted) {
		t.Fatalf("非文本 reasoning_details 应原样存活, 实得 %#v", encrypted)
	}
}

func TestCopyOpenAICompatibleReasoningFields_字段缺失契约(t *testing.T) {
	// 原实现逐字段 `if (source.x !== undefined) target.x = source.x` ——
	// 字段不存在时不得凭空写入。
	target := copyReasoning(map[string]any{"content": "hello"})
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text", "thinking", "thought", "reasoning_details"} {
		if _, exists := target[field]; exists {
			t.Fatalf("源无 %s 时不应写入该字段", field)
		}
	}
	// 非字符串形态的 reasoning(结构化对象)应原样搬运
	structTarget := copyReasoning(map[string]any{"reasoning": map[string]any{"effort": "high"}})
	if !reflect.DeepEqual(structTarget["reasoning"], map[string]any{"effort": "high"}) {
		t.Fatalf("结构化 reasoning 应原样搬运, 实得 %#v", structTarget["reasoning"])
	}
}

func TestCopyOpenAICompatibleReasoningFields_空值防御(t *testing.T) {
	// nil target / nil source 不应 panic
	copyOpenAICompatibleReasoningFields(nil, map[string]any{})
	copyOpenAICompatibleReasoningFields(map[string]any{"reasoning_content": "x"}, nil)
	copyOpenAICompatibleReasoningFields(nil, nil)
}

// ─────────────── opencodeReasoningSanitizer.test.ts 对应用例 ───────────────

func TestIsOpencodeGoProvider(t *testing.T) {
	// 原: "returns true for ollama-cloud / opencode-go / opencode / opencode-zen"
	for _, provider := range []string{"ollama-cloud", "opencode-go", "opencode", "opencode-zen"} {
		if !isOpencodeGoProvider(provider) {
			t.Fatalf("%s 应为 opencode-go 系 provider", provider)
		}
	}
	// 原: "returns false for other providers"
	for _, provider := range []string{"featherless-ai", "glm", "antigravity", ""} {
		if isOpencodeGoProvider(provider) {
			t.Fatalf("%s 不应为 opencode-go 系 provider", provider)
		}
	}
}

func TestStripBooleanReasoning(t *testing.T) {
	// 原: "removes reasoning when it is boolean true"
	bodyTrue := map[string]any{"model": "glm-5.2", "reasoning": true, "messages": []any{}}
	resTrue := stripBooleanReasoning(bodyTrue)
	if _, exists := resTrue["reasoning"]; exists {
		t.Fatal("reasoning=true 应被剥离")
	}
	if resTrue["model"] != "glm-5.2" {
		t.Fatalf("剥离不应影响 model 字段, 实得 %#v", resTrue["model"])
	}
	if !reflect.DeepEqual(resTrue["messages"], []any{}) {
		t.Fatalf("剥离不应影响 messages 字段, 实得 %#v", resTrue["messages"])
	}

	// 原: "removes reasoning when it is boolean false"
	resFalse := stripBooleanReasoning(map[string]any{"model": "glm-5.2", "reasoning": false, "messages": []any{}})
	if _, exists := resFalse["reasoning"]; exists {
		t.Fatal("reasoning=false 应被剥离")
	}

	// 原: "does NOT remove reasoning when it is an object (structured type)"
	resObject := stripBooleanReasoning(map[string]any{"model": "glm-5.2", "reasoning": map[string]any{"effort": "high"}, "messages": []any{}})
	if !reflect.DeepEqual(resObject["reasoning"], map[string]any{"effort": "high"}) {
		t.Fatalf("结构化 reasoning 不应被剥离, 实得 %#v", resObject["reasoning"])
	}

	// 原: "does NOT remove reasoning when it is a string"
	resString := stripBooleanReasoning(map[string]any{"model": "glm-5.2", "reasoning": "high", "messages": []any{}})
	if resString["reasoning"] != "high" {
		t.Fatalf("字符串 reasoning 不应被剥离, 实得 %#v", resString["reasoning"])
	}

	// 原: "returns the same object reference when no reasoning field exists"
	noField := map[string]any{"model": "glm-5.2", "messages": []any{}}
	if got := stripBooleanReasoning(noField); !sameMap(got, noField) {
		t.Fatal("无 reasoning 字段时应返回同一对象引用")
	}

	// 原: "returns the same object reference when reasoning is non-boolean"
	nonBool := map[string]any{"model": "glm-5.2", "reasoning": map[string]any{"effort": "low"}}
	if got := stripBooleanReasoning(nonBool); !sameMap(got, nonBool) {
		t.Fatal("reasoning 非布尔时应返回同一对象引用")
	}

	// 原: "preserves other fields in the body"
	preserved := stripBooleanReasoning(map[string]any{
		"model": "ollama-cloud/glm-5.2", "reasoning": true, "temperature": 0.7, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if preserved["model"] != "ollama-cloud/glm-5.2" || preserved["temperature"] != 0.7 || preserved["stream"] != true {
		t.Fatalf("其余字段应完整保留, 实得 %#v", preserved)
	}
	if !reflect.DeepEqual(preserved["messages"], []any{map[string]any{"role": "user", "content": "hi"}}) {
		t.Fatalf("messages 应完整保留, 实得 %#v", preserved["messages"])
	}
	if _, exists := preserved["reasoning"]; exists {
		t.Fatal("reasoning 应被剥离")
	}

	// 原: "handles empty body object"
	empty := map[string]any{}
	if got := stripBooleanReasoning(empty); !sameMap(got, empty) {
		t.Fatal("空 body 应返回同一对象引用")
	}

	// 原: "returns null/undefined/primitive bodies unchanged"
	if got := stripBooleanReasoning(nil); got != nil {
		t.Fatal("nil body 应原样返回")
	}

	// 原: "does not mutate the original body"
	original := map[string]any{"model": "glm-5.2", "reasoning": true}
	result := stripBooleanReasoning(original)
	if original["reasoning"] != true {
		t.Fatal("原 body 不应被就地修改")
	}
	if _, exists := result["reasoning"]; exists {
		t.Fatal("返回的副本应已移除 reasoning")
	}
}

// sameMap 判断两次调用是否返回同一个底层对象(对应原测试的 assert.equal 引用比较)。
func sameMap(a, b map[string]any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// Go map 不可比较, 用 mangle 后的指针身份间接判断:
	// 借助 reflect.ValueOf 的 Pointer。
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
