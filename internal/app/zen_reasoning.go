package app

import (
	"regexp"
	"strings"
)

// 本文件是 OmniRoute 四个 reasoning 相关模块的 Go 移植, 功能与原实现逐条对齐:
//
//	open-sse/utils/reasoningPlaceholder.ts      → 占位符哨兵与剥离
//	open-sse/utils/reasoningFields.ts           → 推理字段归一(copyOpenAICompatibleReasoningFields)
//	open-sse/utils/reasoningContentInjector.ts  → 请求侧回传占位符注入
//	open-sse/services/opencodeReasoningSanitizer.ts → 剥离 bool reasoning
//
// 移植原则: 逻辑一一对应, 不增不减。原实现未做的事(例如把 reasoning 当正文
// 顶替 content)这里也不做。

// nonAnthropicThinkingPlaceholder 对应 reasoningPlaceholder.ts 的
// NON_ANTHROPIC_THINKING_PLACEHOLDER。
//
// 它是"内部回放哨兵" —— 用于上游要求非空 reasoning 内容、但原始推理摘要不可得
// 的场景。它只是请求脚手架, 永远不是用户可见的推理, 因此响应翻译器必须在
// 向客户端发出事件前把它抑制掉。
const nonAnthropicThinkingPlaceholder = "(prior reasoning summary unavailable)"

// isInternalReasoningPlaceholder 对应 isInternalReasoningPlaceholder。
func isInternalReasoningPlaceholder(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(s) == nonAnthropicThinkingPlaceholder
}

// stripInternalReasoningPlaceholder 对应 stripInternalReasoningPlaceholder。
//
// 模型有时会通过普通的 message.content / delta.content 把哨兵 echo 回来。
// 移除所有出现; 只剩空白时返回 "" 以便调用方整体跳过该帧。
//
// 重要: 这条在流式路径上逐 delta 执行, delta 的首尾空格是有意义的
// (例如 "Hello, " + "world." + " Bye.")。**仅当占位符就是全部内容时**才折叠成
// "" —— 绝不对真实内容做 trim, 否则流式 delta 拼接时空格会被吃掉。
func stripInternalReasoningPlaceholder(value string) string {
	if !strings.Contains(value, nonAnthropicThinkingPlaceholder) {
		return value
	}
	stripped := strings.ReplaceAll(value, nonAnthropicThinkingPlaceholder, "")
	if strings.TrimSpace(stripped) == "" {
		return ""
	}
	return stripped
}

// asReasoningRecord 对应 asReasoningRecord: 非对象(含数组、null)一律给空对象。
func asReasoningRecord(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	m, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// nonEmptyString 对应 nonEmptyString: 仅非空字符串有值。
func nonEmptyString(value any) string {
	s, ok := value.(string)
	if !ok || len(s) == 0 {
		return ""
	}
	return s
}

// extractReasoningDetailsText 对应 extractReasoningDetailsText。
// 逐项取 text, 取不到再取 content, 拼接。
func extractReasoningDetailsText(value any) string {
	record := asReasoningRecord(value)
	details, ok := record["reasoning_details"].([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, detail := range details {
		item := asReasoningRecord(detail)
		if s := nonEmptyString(item["text"]); s != "" {
			sb.WriteString(s)
			continue
		}
		sb.WriteString(nonEmptyString(item["content"]))
	}
	return sb.String()
}

// getReadableReasoningValue 对应 getReadableReasoningValue:
// 标准可读字段 reasoning_content → reasoning。
func getReadableReasoningValue(value any) string {
	record := asReasoningRecord(value)
	if s := nonEmptyString(record["reasoning_content"]); s != "" {
		return s
	}
	return nonEmptyString(record["reasoning"])
}

// getUnsupportedReasoningValue 对应 getUnsupportedReasoningValue:
// 非标准字段 reasoning_text → thinking → thought → reasoning_details。
func getUnsupportedReasoningValue(value any) string {
	record := asReasoningRecord(value)
	if s := nonEmptyString(record["reasoning_text"]); s != "" {
		return s
	}
	if s := nonEmptyString(record["thinking"]); s != "" {
		return s
	}
	if s := nonEmptyString(record["thought"]); s != "" {
		return s
	}
	return extractReasoningDetailsText(record)
}

// getAnyReasoningValue 对应 getAnyReasoningValue。
func getAnyReasoningValue(value any) string {
	if s := getReadableReasoningValue(value); s != "" {
		return s
	}
	return getUnsupportedReasoningValue(value)
}

// hasUnsupportedReasoningSignal 对应 hasUnsupportedReasoningSignal。
func hasUnsupportedReasoningSignal(value any) bool {
	record := asReasoningRecord(value)
	if getReadableReasoningValue(record) != "" {
		return false
	}
	if nonEmptyString(record["reasoning_text"]) != "" ||
		nonEmptyString(record["thinking"]) != "" ||
		nonEmptyString(record["thought"]) != "" {
		return true
	}
	details, ok := record["reasoning_details"].([]any)
	return ok && len(details) > 0
}

// hasAnyReasoningSignal 对应 hasAnyReasoningSignal。
func hasAnyReasoningSignal(value any) bool {
	record := asReasoningRecord(value)
	if getReadableReasoningValue(record) != "" {
		return true
	}
	if nonEmptyString(record["reasoning_text"]) != "" ||
		nonEmptyString(record["thinking"]) != "" ||
		nonEmptyString(record["thought"]) != "" {
		return true
	}
	details, ok := record["reasoning_details"].([]any)
	return ok && len(details) > 0
}

// strippableReasoningFields 对应 STRIPPABLE_REASONING_FIELDS。
var strippableReasoningFields = []string{
	"reasoning_content",
	"reasoning",
	"reasoning_text",
	"thinking",
	"thought",
}

// stripPlaceholderFromField 对应 stripPlaceholderFromField。
//
// 只在该字段存在且为字符串时处理:
//   - 剥离后为空 → 删除字段, 返回 true(调用方可区分"被移除"与"本来就没有");
//   - 剥离后与原值不同 → 写回缩短后的值, 返回 false;
//   - 不含占位符 → 不动, 返回 false;
//   - 字段缺失或非字符串 → 返回 false。
func stripPlaceholderFromField(target map[string]any, field string) bool {
	value, ok := target[field].(string)
	if !ok {
		return false
	}
	stripped := stripInternalReasoningPlaceholder(value)
	if stripped == "" {
		delete(target, field)
		return true
	}
	if stripped != value {
		target[field] = stripped
	}
	return false
}

// copyOpenAICompatibleReasoningFields 对应 copyOpenAICompatibleReasoningFields。
//
// 把 source 里的推理字段照搬进 target(存在才搬, 不覆盖成空), 然后在 target 上:
//  1. 若无可读推理值, 把 source 的非标准推理值镜像到 target.reasoning_content;
//  2. 逐字段剥离内部占位符;
//  3. 净化 reasoning_details 数组 —— 逐项剥离 text/content, 原本有文本、
//     剥离后两者皆空的项丢弃; 保留只带 data 的非文本项(如 encrypted);
//     数组清空后删除该字段。
func copyOpenAICompatibleReasoningFields(source, target map[string]any) {
	if source == nil || target == nil {
		return
	}
	if v, ok := source["reasoning_content"]; ok {
		target["reasoning_content"] = v
	}
	if v, ok := source["reasoning"]; ok {
		target["reasoning"] = v
	}
	if v, ok := source["reasoning_text"]; ok {
		target["reasoning_text"] = v
	}
	if v, ok := source["thinking"]; ok {
		target["thinking"] = v
	}
	if v, ok := source["thought"]; ok {
		target["thought"] = v
	}
	if v, ok := source["reasoning_details"].([]any); ok {
		target["reasoning_details"] = v
	}

	if getReadableReasoningValue(target) == "" {
		if mirrored := getUnsupportedReasoningValue(source); mirrored != "" {
			target["reasoning_content"] = mirrored
		}
	}

	// 内部回放占位符是请求脚手架, 从不是真实推理 —— 模型会把它 echo 回来,
	// 污染客户端历史与缓存。凡是要转发给客户端的内容都要剥掉, 包括非标准
	// 推理字段(reasoning_text / thinking / thought)以及 reasoning_details 项。
	for _, field := range strippableReasoningFields {
		stripPlaceholderFromField(target, field)
	}

	if details, ok := target["reasoning_details"].([]any); ok {
		cleaned := make([]any, 0, len(details))
		for _, detail := range details {
			record := asReasoningRecord(detail)
			next := make(map[string]any, len(record))
			for k, v := range record {
				next[k] = v
			}
			// 记录该项原本是否带 text/content, 这样只带 data 的非文本项
			// (例如 reasoning.encrypted)能原样存活。
			_, hadText := next["text"].(string)
			_, hadContent := next["content"].(string)
			stripPlaceholderFromField(next, "text")
			stripPlaceholderFromField(next, "content")
			_, textPresent := next["text"]
			_, contentPresent := next["content"]
			textGone := !textPresent
			contentGone := !contentPresent
			if (hadText || hadContent) && textGone && contentGone {
				continue
			}
			cleaned = append(cleaned, next)
		}
		if len(cleaned) == 0 {
			delete(target, "reasoning_details")
		} else {
			target["reasoning_details"] = cleaned
		}
	}
}

// ───────────────────────── 请求侧: 回传占位符注入 ─────────────────────────

// reasoningEchoPlaceholder 对应 reasoningContentInjector.ts 的 PLACEHOLDER。
//
// 注意它与 nonAnthropicThinkingPlaceholder 是**两个不同的值**: 这里是注入用的
// 一个空格, 那里是剥离时匹配的哨兵串。原实现如此, 此处照搬。
const reasoningEchoPlaceholder = " "

// thinkingModelPatterns 对应 THINKING_MODEL_PATTERNS。
// 大小写不敏感地匹配解析后的模型 ID。
var thinkingModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)deepseek`),
	regexp.MustCompile(`(?i)\bkimi\b`),
	regexp.MustCompile(`(?i)\bk2\b`), // moonshot kimi k2 family alias
	regexp.MustCompile(`(?i)\bminimax\b`),
	regexp.MustCompile(`(?i)\bmimo\b`), // xiaomi-tokenplan mimo family
}

// k3AuthenticReasoningPattern 对应 K3_AUTHENTIC_REASONING_PATTERN。
var k3AuthenticReasoningPattern = regexp.MustCompile(`(?i)(?:^|/)(?:kimi-)?k3(?:$|-)`)

// nativeK27AuthenticReasoningPattern 对应 NATIVE_K27_AUTHENTIC_REASONING_PATTERN。
var nativeK27AuthenticReasoningPattern = regexp.MustCompile(`(?i)(?:^|/)kimi-k2\.7-code(?:$|-)`)

// requiresAuthenticReasoningContent 对应 requiresAuthenticReasoningContent。
//
// K3 无论由哪个 provider 提供都要求真实推理; 原生 Moonshot K2.7 保持同样的
// preserved-thinking 契约。空协议标记只在客户端内容与回放都缺失后才有效。
func requiresAuthenticReasoningContent(provider any, model any) bool {
	modelStr, _ := model.(string)
	normalizedModel := strings.TrimSpace(modelStr)
	if k3AuthenticReasoningPattern.MatchString(normalizedModel) {
		return true
	}
	providerStr, _ := provider.(string)
	normalizedProvider := strings.ToLower(strings.TrimSpace(providerStr))
	return (normalizedProvider == "moonshot" || normalizedProvider == "kimi") &&
		nativeK27AuthenticReasoningPattern.MatchString(normalizedModel)
}

// isThinkingMessageModel 对应 isThinkingMessageModel。
func isThinkingMessageModel(model any) bool {
	s, ok := model.(string)
	if !ok || s == "" {
		return false
	}
	for _, re := range thinkingModelPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// shouldInjectReasoningContentPlaceholder 对应 shouldInjectReasoningContentPlaceholder。
func shouldInjectReasoningContentPlaceholder(provider any, model any) bool {
	providerStr, _ := provider.(string)
	normalizedProvider := strings.ToLower(strings.TrimSpace(providerStr))
	return (normalizedProvider == "moonshot" || normalizedProvider == "kimi") &&
		!requiresAuthenticReasoningContent(normalizedProvider, model) &&
		isThinkingMessageModel(model)
}

// hasNonEmptyReasoningContent 对应 hasNonEmptyReasoningContent。
func hasNonEmptyReasoningContent(message map[string]any) bool {
	s, ok := message["reasoning_content"].(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(s) != ""
}

// isAssistantMessage 对应 isAssistantMessage。
func isAssistantMessage(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	role, _ := m["role"].(string)
	if role != "assistant" {
		return nil, false
	}
	return m, true
}

// injectReasoningContentForThinkingModel 对应 injectReasoningContentForThinkingModel。
//
// 给 body.messages 里每条缺 reasoning_content 的 assistant 消息注入占位符。
// 无需改动时返回原对象; 需要改动时返回浅拷贝的 body 与新 messages 数组。
// body 形状不符(非对象 / messages 非数组)时原样返回 —— 防御式处理。
func injectReasoningContentForThinkingModel(body any) any {
	record, ok := body.(map[string]any)
	if !ok {
		return body
	}
	messages, ok := record["messages"].([]any)
	if !ok {
		return body
	}

	modified := false
	out := make([]any, 0, len(messages))
	for _, message := range messages {
		msg, ok := isAssistantMessage(message)
		if !ok {
			out = append(out, message)
			continue
		}
		if hasNonEmptyReasoningContent(msg) {
			out = append(out, message)
			continue
		}
		modified = true
		cloned := make(map[string]any, len(msg)+1)
		for k, v := range msg {
			cloned[k] = v
		}
		cloned["reasoning_content"] = reasoningEchoPlaceholder
		out = append(out, cloned)
	}

	if !modified {
		return body
	}
	next := make(map[string]any, len(record))
	for k, v := range record {
		next[k] = v
	}
	next["messages"] = out
	return next
}

// ───────────────────── 请求侧: 剥离 bool 型 reasoning ─────────────────────

// opencodeGoProviders 对应 OPENCODE_GO_PROVIDERS。
var opencodeGoProviders = map[string]bool{
	"ollama-cloud": true,
	"opencode-go":  true,
	"opencode":     true,
	"opencode-zen": true,
}

// isOpencodeGoProvider 对应 isOpencodeGoProvider。
func isOpencodeGoProvider(provider string) bool {
	return opencodeGoProviders[provider]
}

// stripBooleanReasoning 对应 stripBooleanReasoning。
//
// 无需改动时返回原对象; 需要移除时返回删掉该字段的浅拷贝。
func stripBooleanReasoning(body map[string]any) map[string]any {
	if body == nil {
		return body
	}
	reasoning, ok := body["reasoning"]
	if !ok {
		return body
	}
	// 仅当 reasoning 是布尔才剥离 —— 对象/字符串形态对 Go 结构体是合法的,
	// 应当原样转发。
	if _, isBool := reasoning.(bool); !isBool {
		return body
	}
	next := make(map[string]any, len(body))
	for k, v := range body {
		if k == "reasoning" {
			continue
		}
		next[k] = v
	}
	return next
}
