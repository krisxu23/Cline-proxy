package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// 逐字照抄 OmniRoute:
//
//	open-sse/utils/streamReadiness.ts（614 行）
//
// ★ 本文件实现**提交前预检**（ensureStreamReadiness 的判定核心）：
// 上游可能回 200 + text/event-stream 却**只发 ping / error-only / 空帧**
// 就结束。旧实现（stream_early_eof.go 的 probeStreamFirstEvent）只看
// `data:` 行非空，会把 `data: {"error":{"message":"rate limit"}}` 这种
// **error-only 帧**误判为"流已就绪"，导致空答案被当成功提交给客户端，
// 候选链也不再换站 —— 用户侧表现为"模型返回空内容 / 任务无声中断"。
//
// 参考实现的判定链（本文件逐字复刻）：
//   1. 跳过 ping/keepalive/heartbeat 事件（event: 行 + data 的 type 字段都查）
//   2. 跳过空 data 与 [DONE]
//   3. JSON 解析成功 → hasNonPingStructuredPayload：
//      a. 空对象 → false
//      b. error-only 帧（有 error 键、无任何 CONTENT_BEARING_KEYS）→ false
//         并顺带把 error.message 存进 upstreamDiagnostic
//      c. 其余 → true
//   4. JSON 解析失败 → 非空字符串即 true（非 JSON data 仍是有效内容）
//
// ★ 有意不接的部分：`ensureStreamReadiness` 的**外层读循环**
// （readWithTimeout / prependBufferedChunks / Response 重建）在 Go 侧
// 由 stream_early_eof.go 的 prefixedBody + idle reader 承担，
// 本文件只提供**判定函数**（纯函数，无 IO 副作用，可直接单测）。

// --- 常量（照抄 streamReadiness.ts） --------------------------------------

// isPingEventType 照抄 :94-96 `/^(?:ping|keepalive|heartbeat)$/i`。
var pingEventTypeRe = regexp.MustCompile(`^(?:ping|keepalive|heartbeat)$`)

func isPingEventType(t string) bool {
	return pingEventTypeRe.MatchString(strings.ToLower(t))
}

// CONTENT_BEARING_KEYS 照抄 :111-123。
//
// 帧携带（或开始携带）实际模型输出的键 —— 与只带 `{error:{...}}` 的帧
// 相对。只发 error-only 帧的流（如 CLI 透传执行器的中途 spawn 失败，#7503）
// 绝不能被判为"已就绪"：当成就绪会让畸形帧以假 200 到达客户端，
// 并阻断候选链换到下一个候选。
var contentBearingKeys = []string{
	"choices",
	"candidates",
	"content_block",
	"delta",
	"output",
	"response",
	"parts",
	"tool_calls",
	"tool_use",
	"function_call",
	"function_call_output",
}

// usefulValueKeys 照抄 :45-62 —— 第一组：值本身有意义（嵌套递归）。
var usefulValueKeys = []string{
	"content",
	"text",
	"delta",
	"reasoning_content",
	"reasoning",
	// Mistral/Magistral thinking 数组与 StepFun/OpenRouter reasoning_details
	// 是有效模型输出 —— 没有它们，纯 reasoning 流会被误判为"无有效内容"
	// 并变成虚假 502（#2520）。
	"thinking",
	"reasoning_details",
	"partial_json",
	"arguments",
	"name",
	"thought",
	"error",
	"executableCode",
	"codeExecutionResult",
}

// usefulNestedKeys 照抄 :69-84 —— 第二组：整体递归判。
var usefulNestedKeys = []string{
	"tool_calls",
	"tool_use",
	"function",
	"functionCall",
	"function_call",
	"function_call_output",
	"output",
	"content_block",
	"response",
	"choices",
	"candidates",
	"parts",
}

// LEGIT_EMPTY_TERMINAL_REASONS 已在 proxy_stream.go:478 声明并照抄同一出处
// （streamReadiness.ts:166-172），此处直接复用，不重复声明。

// terminalReasonRe 照抄 :174 /"(?:finish_reason|stop_reason)"\s*:\s*"([^"]+)"/g
var terminalReasonRe = regexp.MustCompile(`"(?:finish_reason|stop_reason)"\s*:\s*"([^"]+)"`)

// sseFieldLineRe 照抄 :176 /(?:^|\r?\n)\s*(?:data|event):/
var sseFieldLineRe = regexp.MustCompile(`(?:^|\r?\n)\s*(?:data|event):`)

// --- 工具函数 --------------------------------------------------------------

// sanitizeErrorMessage 照抄 open-sse/utils/error.ts:63-74。
//
// 参考实现原文（逐字）：
//
//	export function sanitizeErrorMessage(message: unknown): string {
//	  let str = typeof message === "string" ? message : String(message ?? "");
//	  if (str.length > MAX_ERROR_LEN) str = str.slice(0, MAX_ERROR_LEN);
//	  const nl = str.indexOf("\n");
//	  const firstLine = nl >= 0 ? str.slice(0, nl) : str;
//	  const parts = firstLine.split(/(\s+)/);
//	  for (let i = 0; i < parts.length; i++) {
//	    if (looksLikeAbsolutePath(parts[i])) parts[i] = "<path>";
//	  }
//	  return redactSensitiveErrorText(parts.join(""));
//	}
//
// 逐字照抄（不做"Go 侧简化"）：取首行 + 截断 + 源码路径脱敏 + 敏感凭据脱敏。
const maxErrorLen = 4096

// SOURCE_EXT 照抄 error.ts:24。
var sourceExts = []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"}

func isAsciiLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// looksLikeAbsolutePath 照抄 error.ts:26-40。
//
// ★ 关键细节（旧"简化"版完全漏掉）：token 必须以 .ts/.tsx/.js/.jsx/.mjs/.cjs
// 结尾（:33-39）—— "…/file.go:12" 的扩展名是 go，**不脱敏**；只有源码文件路径
// 才脱敏。长度边界 4..2048；POSIX 以 / 开头，Windows 是 X: 形式。
func looksLikeAbsolutePath(tok string) bool {
	if len(tok) < 4 || len(tok) > 2048 {
		return false
	}
	isPosix := tok[0] == '/'
	isWindows := len(tok) > 2 && tok[1] == ':' && isAsciiLetter(tok[0])
	if !isPosix && !isWindows {
		return false
	}
	dot := strings.LastIndex(tok, ".")
	if dot <= 0 || dot == len(tok)-1 {
		return false
	}
	ext := tok[dot+1:]
	// 扩展名按 ":" 截断（去掉 :line[:col] 后缀，:37）
	if i := strings.IndexByte(ext, ':'); i >= 0 {
		ext = ext[:i]
	}
	ext = strings.ToLower(ext)
	for _, e := range sourceExts {
		if ext == e {
			return true
		}
	}
	return false
}

// splitErrorTokens 对位 error.ts:69 的 firstLine.split(/(\s+)/)：
// 返回交替的「非空白 token / 空白分隔符」，join("") 后还原原始空白。
// （JS 的 split 带捕获分组会把空白串也保留为元素，Go 标准库无直接等价物。）
func splitErrorTokens(s string) []string {
	runes := []rune(s)
	var parts []string
	i := 0
	for i < len(runes) {
		j := i
		for j < len(runes) && !unicode.IsSpace(runes[j]) {
			j++
		}
		parts = append(parts, string(runes[i:j]))
		i = j
		for i < len(runes) && unicode.IsSpace(runes[i]) {
			i++
		}
		if i > j {
			parts = append(parts, string(runes[j:i]))
		}
	}
	return parts
}

// redactSensitiveErrorText 照抄 error.ts:42-54（四条替换链）。
var dataUrlRe = regexp.MustCompile(`(?i)data:[^,\s]+;base64,[A-Za-z0-9+/=_-]+`)
var bearerRe = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
var credQuotedRe = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|access[_-]?token|authorization|cookie|secret)["']?\s*[:=]\s*["'])[^"']*["']`)
var credUnquotedRe = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|access[_-]?token|authorization|cookie|secret)["']?\s*[:=]\s*)[^"',\s}]+`)

func redactSensitiveErrorText(value string) string {
	value = dataUrlRe.ReplaceAllString(value, "[REDACTED_DATA_URL]")
	value = bearerRe.ReplaceAllString(value, "${1} [REDACTED]")
	value = credQuotedRe.ReplaceAllString(value, "${1}[REDACTED]${2}")
	value = credUnquotedRe.ReplaceAllString(value, "${1}[REDACTED]")
	return value
}

func sanitizeErrorMessage(message any) string {
	str, ok := message.(string)
	if !ok {
		if message == nil {
			return ""
		}
		// 非 string 类型按 JS String(x ?? "") 语义转
		str = fmt.Sprintf("%v", message)
	}
	if len(str) > maxErrorLen {
		str = str[:maxErrorLen]
	}
	// 取首行（:66-67）
	if nl := strings.Index(str, "\n"); nl >= 0 {
		str = str[:nl]
	}
	// 按空白分词，保留分隔符（:69 的 /(\s+)/ 捕获分组）
	parts := splitErrorTokens(str)
	for i, p := range parts {
		if looksLikeAbsolutePath(p) {
			parts[i] = "<path>"
		}
	}
	return redactSensitiveErrorText(strings.Join(parts, ""))
}

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func hasNonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && len(s) > 0
}

// hasUsefulValue 照抄 :32-87（递归）。
func hasUsefulValue(v any) bool {
	if hasNonEmptyString(v) {
		return true
	}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if hasUsefulValue(e) {
				return true
			}
		}
		return false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}

	// Responses compaction 条目本身就是该轮输出（:37-43）。
	// ★ 不是对 encrypted_content 键的一揽子豁免 —— 单独一个加密 reasoning
	//   条目不是用户可见输出，仍须让 #8649 空内容守卫触发。
	if m["type"] == "compaction" && hasNonEmptyString(m["encrypted_content"]) {
		return true
	}

	for _, key := range usefulValueKeys {
		candidate, exists := m[key]
		if !exists {
			continue
		}
		if hasNonEmptyString(candidate) {
			return true
		}
		if _, ok := candidate.([]any); ok {
			if hasUsefulValue(candidate) {
				return true
			}
			continue
		}
		if isRecord(candidate) {
			if hasUsefulValue(candidate) {
				return true
			}
		}
	}

	for _, key := range usefulNestedKeys {
		if _, exists := m[key]; exists {
			if hasUsefulValue(m[key]) {
				return true
			}
		}
	}

	return false
}

func hasUsefulJsonPayload(payload any) bool {
	if !isRecord(payload) {
		return false
	}
	return hasUsefulValue(payload)
}

// getPayloadType 照抄 :98-102。
func getPayloadType(payload any, eventType string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return eventType
	}
	for _, key := range []string{"type", "event", "object"} {
		if t, ok := m[key].(string); ok {
			return t
		}
	}
	return eventType
}

// isErrorOnlyStructuredPayload 照抄 :125-128。
func isErrorOnlyStructuredPayload(payload map[string]any) bool {
	if _, hasErr := payload["error"]; !hasErr {
		return false
	}
	for _, key := range contentBearingKeys {
		if _, has := payload[key]; has {
			return false
		}
	}
	return true
}

// hasNonPingStructuredPayload 照抄 :130-139。
func hasNonPingStructuredPayload(payload any, eventType string) bool {
	typ := getPayloadType(payload, eventType)
	if isPingEventType(eventType) || isPingEventType(typ) {
		return false
	}
	if arr, ok := payload.([]any); ok {
		return len(arr) > 0
	}
	if m, ok := payload.(map[string]any); ok {
		if len(m) == 0 {
			return false
		}
		return !isErrorOnlyStructuredPayload(m)
	}
	return payload != nil
}

// --- 流就绪信号状态机（照抄 :316-398） --------------------------------------

// streamReadinessState 对位 StreamReadinessSignalState（:316-321）。
type streamReadinessState struct {
	currentEvent       string
	dataLines          []string
	pendingLine        string
	upstreamDiagnostic string
}

func resetCurrentEvent(st *streamReadinessState) {
	st.currentEvent = ""
	st.dataLines = nil
}

// processStreamReadinessEvent 照抄 :328-352。
// 返回 (是否就绪)。
func processStreamReadinessEvent(st *streamReadinessState) bool {
	eventType := st.currentEvent
	data := strings.TrimSpace(strings.Join(st.dataLines, "\n"))
	resetCurrentEvent(st)

	if isPingEventType(eventType) || data == "" || data == "[DONE]" {
		return false
	}

	var payload any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		// JSON 解析失败 → 非空 data 即有效（非 JSON data 仍是有效内容）
		return len(data) > 0
	}

	// 捕获 error-only 帧的诊断信息（:337-347）
	if st.upstreamDiagnostic == "" {
		if m, ok := payload.(map[string]any); ok && isErrorOnlyStructuredPayload(m) {
			rawMsg := ""
			if s, ok := payload.(map[string]any)["error"].(string); ok {
				rawMsg = s
			} else if em, ok := payload.(map[string]any)["error"].(map[string]any); ok {
				if s, ok := em["message"].(string); ok {
					rawMsg = s
				}
			}
			if d := strings.TrimSpace(sanitizeErrorMessage(rawMsg)); d != "" {
				st.upstreamDiagnostic = d
			}
		}
	}

	if !hasNonPingStructuredPayload(payload, eventType) {
		return false
	}
	// ★ 收紧(2026-09-17 审查 P0-1): 纯脚手架帧不算"流已就绪"。
	//
	// 原判据只要"非空对象、非 error-only"就算就绪, 于是
	//   {"choices":[{"delta":{"role":"assistant"}}]}       (role 骨架帧)
	//   {"choices":[{"delta":{},"finish_reason":"stop"}]}   (终止帧)
	// 都能骗过它 → 探测提交 → 整条流零产出 → 客户端拿到"成功但空"的回合,
	// 不报错不重试, 任务静默中断。
	//
	// 提交前是**唯一能换站重试**的时机(提交后响应头已发出, 只能报错), 所以
	// 这道判据必须严。判据本体在 stream_delivery.go 的 readinessScaffoldingOnly,
	// 与流级产出判据同源 —— 只处理 OpenAI 形态, 其余形态不介入。
	if readinessScaffoldingOnly(payload) {
		return false
	}
	return true
}

// processStreamReadinessLine 照抄 :354-370。
func processStreamReadinessLine(st *streamReadinessState, line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return processStreamReadinessEvent(st)
	}
	if strings.HasPrefix(trimmed, ":") {
		return false
	}
	if strings.HasPrefix(trimmed, "event:") {
		st.currentEvent = strings.TrimSpace(trimmed[len("event:"):])
		return false
	}
	if strings.HasPrefix(trimmed, "data:") {
		st.dataLines = append(st.dataLines, strings.TrimLeft(trimmed[len("data:"):], " \t"))
	}
	return false
}

// appendStreamReadinessSignal 照抄 :372-381。
func appendStreamReadinessSignal(st *streamReadinessState, chunk string) bool {
	combined := st.pendingLine + chunk
	lines := strings.Split(combined, "\n")
	// 最后一行可能不完整，留着
	st.pendingLine = lines[len(lines)-1]
	lines = lines[:len(lines)-1]

	// 处理 \r\n：split 后每行末尾可能带 \r
	for _, line := range lines {
		if processStreamReadinessLine(st, strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// finishStreamReadinessSignal 照抄 :383-387。
func finishStreamReadinessSignal(st *streamReadinessState) bool {
	if st.pendingLine != "" {
		if processStreamReadinessLine(st, strings.TrimRight(st.pendingLine, "\r")) {
			return true
		}
		st.pendingLine = ""
	}
	return processStreamReadinessEvent(st)
}

// hasStreamReadinessSignal 照抄 :389-398（导出，供探针与测试使用）。
func hasStreamReadinessSignal(text string) bool {
	st := &streamReadinessState{}
	if appendStreamReadinessSignal(st, text) {
		return true
	}
	return finishStreamReadinessSignal(st)
}

// --- 客户端侧内容观测（照抄 :231-314） --------------------------------------

// frameHasStructuredStreamError 照抄 :196-229。
//
// 当 SSE 帧已携带结构化上游/客户端错误（OpenAI `error`、Claude
// `event:error`/`type:error`、Responses `response.failed`）时返回 true。
// #8649 用它避免在执行器已发出可操作错误后编造"Provider returned empty content"
// （Claude #3685 / readiness #8972 对齐）。
func frameHasStructuredStreamError(frame string) bool {
	lines := strings.Split(frame, "\n")
	eventType := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		if strings.HasPrefix(trimmed, "event:") {
			eventType = strings.TrimSpace(trimmed[len("event:"):])
			if strings.EqualFold(eventType, "error") {
				return true
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}

		data := strings.TrimSpace(trimmed[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}

		var parsed any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue // 非 JSON data 行不是结构化错误
		}
		m, ok := parsed.(map[string]any)
		if !ok {
			continue
		}
		typ := getPayloadType(parsed, eventType)
		if typ == "error" || typ == "response.failed" || eventType == "response.failed" {
			return true
		}
		if isSubstantiveErrorValue(parsed.(map[string]any)["error"]) {
			return true
		}
		if nested, ok := m["response"].(map[string]any); ok {
			if nested["status"] == "failed" && nested["error"] != nil {
				return true
			}
		}
	}

	return false
}

// isSubstantiveErrorValue 照抄 :179-188（同 combo isSubstantiveErrorError 语义）：
// 非空字符串或非空对象。
func isSubstantiveErrorValue(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	if m, ok := v.(map[string]any); ok {
		if hasNonEmptyString(m["message"]) {
			return true
		}
		return len(m) > 0
	}
	return v == true
}

// hasUsefulStreamContent 照抄 :141-161（导出，供测试使用）。
func hasUsefulStreamContent(text string) bool {
	lines := strings.Split(text, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		if pingEventLineRe.MatchString(trimmed) {
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}

		data := strings.TrimSpace(trimmed[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}

		var payload any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			// 解析失败 → 非空即有效
			return len(data) > 0
		}
		if hasUsefulJsonPayload(payload) {
			return true
		}
	}

	return false
}

// pingEventLineRe 对位 :147 `/^event:\s*(?:ping|keepalive)$/i`
var pingEventLineRe = regexp.MustCompile(`(?i)^event:\s*(?:ping|keepalive)$`)

// --- StreamContentWatcher（照抄 :231-314） ---------------------------------

// streamContentWatcher 对位 StreamContentWatcher（:231-252）：
// 观察客户端侧 SSE 流是否真的产出了模型输出。
//
// 帧在空行边界处缓冲，使跨两个网络 chunk 的 delta 仍被当一个 payload 扫描。
// 缓冲有上限 —— 超过上限的单帧被分段扫描，只可能丢失"检测到内容"的精度
// （偏向 sawContent=true），不会偏向假空。
const streamWatcherMaxBuffered = 64 * 1024

type streamContentWatcher struct {
	pending    string
	content    bool
	legitEmpty bool
	sse        bool
	sawErr     bool
}

func newStreamContentWatcher() *streamContentWatcher {
	return &streamContentWatcher{}
}

// inspect 照抄 :276-288。
func (w *streamContentWatcher) inspect(frame string) {
	if frame == "" {
		return
	}
	if !w.sse && sseFieldLineRe.MatchString(frame) {
		w.sse = true
	}
	if !w.sawErr && frameHasStructuredStreamError(frame) {
		w.sawErr = true
	}
	if !w.content && hasUsefulStreamContent(frame) {
		w.content = true
	}
	if w.legitEmpty {
		return
	}
	for _, m := range terminalReasonRe.FindAllStringSubmatch(frame, -1) {
		if legitEmptyTerminalReasons[m[1]] {
			w.legitEmpty = true
			return
		}
	}
}

// Note 照抄 :291-304。喂入一段已解码的客户端流切片，可传部分帧。
func (w *streamContentWatcher) Note(text string) {
	if text == "" {
		return
	}
	w.pending += text
	for {
		idx := strings.Index(w.pending, "\n\n")
		if idx < 0 {
			break
		}
		w.inspect(w.pending[:idx])
		// 跳过边界本身（含 \r\n\r?\n）
		rest := w.pending[idx+2:]
		w.pending = strings.TrimLeft(rest, "\r")
	}
	if len(w.pending) > streamWatcherMaxBuffered {
		w.inspect(w.pending)
		w.pending = ""
	}
}

// Finish 照抄 :305-308。流结束时冲刷缓冲的尾部帧。
func (w *streamContentWatcher) Finish() {
	w.inspect(w.pending)
	w.pending = ""
}

func (w *streamContentWatcher) SawContent() bool            { return w.content }
func (w *streamContentWatcher) SawLegitEmptyTerminal() bool { return w.legitEmpty }
func (w *streamContentWatcher) SawSseFrame() bool           { return w.sse }
func (w *streamContentWatcher) SawError() bool              { return w.sawErr }
