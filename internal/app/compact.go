package app

import (
	"context"
	"encoding/json"
	"fmt"
	"free-router/internal/kit"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// opencode 官方会话压缩 (SessionCompaction) 的 Go 移植
// 源码: packages/core/src/session/compaction.ts + packages/opencode/src/session/compaction.ts
// 机制: 超限时 select() 尾部预算选择 -> buildPrompt() 锚定摘要 -> LLM 生成摘要
//       -> 重组为 [摘要 + recent 尾部] 继续会话,下次压缩增量更新摘要
// ============================================================================

const (
	summaryOutputTokens = 4096 // SUMMARY_OUTPUT_TOKENS
	toolOutputMaxChars  = 2000 // TOOL_OUTPUT_MAX_CHARS
)

// summaryTemplate 官方 SUMMARY_TEMPLATE 原文
const summaryTemplate = `Output exactly the Markdown structure shown inside <template> and keep the section order unchanged. Do not include the <template> tags in your response.
<template>
## Objective
- [one or two brief sentences describing what the user is trying to accomplish]

## Important Details
- [constraints/preferences, decisions and why, important facts/assumptions, exact context needed to continue, or "(none)"]

## Work State
### Completed
- [finished work, verified facts, or changes made; otherwise "(none)"]

### Active
- [current work, partial changes, or investigation state; otherwise "(none)"]

### Blocked
- [blockers, failing commands, or unknowns; otherwise "(none)"]

## Next Move
1. [immediate concrete action, or "(none)"]
2. [next action if known, or "(none)"]

## Relevant Files
- [file or directory path: why it matters, or "(none)"]
</template>

Rules:
- Keep every section, even when empty.
- Use terse bullets, not prose paragraphs.
- Preserve exact file paths, symbols, commands, error strings, URLs, and identifiers when known.
- Do not mention the summary process or that context was compacted.`

// compactState 会话压缩状态(增量摘要记忆)
type compactState struct {
	summary string // 上次生成的锚定摘要
	recent  string // 上次保留的 recent 尾部文本
	updated time.Time
}

var (
	compactStates   = make(map[string]*compactState)
	compactStatesMu sync.Mutex
)

const (
	// maxCompactStates 条目总数上限。key 来自客户端可控的 header/body, 无上限时
	// 每请求换一个伪造 session id 即可让 map 在 24h 内线性膨胀(每条含 summary+
	// recent 文本), 故必须封顶, 超限按 updated 淘汰最旧(LRU)。
	maxCompactStates = 1024
	// compactSessionIDMaxLen session key 长度上限(UUID 36 / ses_ 前缀均远小于此)。
	compactSessionIDMaxLen = 128
)

// validCompactSessionID 校验 session key 格式: 只接受有限长度的
// [A-Za-z0-9_-](UUID / ses_xxx / 客户端短 id 形态)。不合法一律当作
// "无 session", 不建条目 —— key 直接来自客户端, 不校验则任意字符串都能成为 map 键。
func validCompactSessionID(sid string) bool {
	if sid == "" || len(sid) > compactSessionIDMaxLen {
		return false
	}
	for i := 0; i < len(sid); i++ {
		c := sid[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// evictCompactStatesLocked 把条目数压回上限内, 按 updated 淘汰最旧。
// 调用方必须已持有 compactStatesMu(避免引入 map race)。
func evictCompactStatesLocked() {
	for len(compactStates) > maxCompactStates {
		oldestKey := ""
		var oldest time.Time
		for k, v := range compactStates {
			if oldestKey == "" || v.updated.Before(oldest) {
				oldestKey, oldest = k, v.updated
			}
		}
		if oldestKey == "" {
			return
		}
		delete(compactStates, oldestKey)
	}
}

// ============ 消息序列化(官方 serialize 移植) ============

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func msgText(m map[string]any) string {
	content := m["content"]
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := []string{}
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncateMsg(s string) string {
	if len(s) <= toolOutputMaxChars {
		return s
	}
	return s[:toolOutputMaxChars] + "\n[truncated]"
}

// serializeMsg 单条消息 -> 一行文本(官方 serialize 格式)
func serializeMsg(m map[string]any) string {
	switch strField(m, "role") {
	case "user":
		return "[User]: " + msgText(m)
	case "system":
		return "[System update]: " + msgText(m)
	case "tool":
		return "[Tool result]: " + truncateMsg(msgText(m))
	case "assistant":
		lines := []string{}
		if t := msgText(m); t != "" {
			lines = append(lines, "[Assistant]: "+t)
		}
		if r := strField(m, "reasoning_content"); r != "" {
			lines = append(lines, "[Assistant reasoning]: "+r)
		}
		if tc, ok := m["tool_calls"].([]any); ok {
			for _, c := range tc {
				if cm, ok := c.(map[string]any); ok {
					fn, _ := cm["function"].(map[string]any)
					name := ""
					args := ""
					if fn != nil {
						name, _ = fn["name"].(string)
						if a, ok := fn["arguments"].(string); ok {
							args = a
						}
					}
					if name == "" {
						continue
					}
					lines = append(lines, fmt.Sprintf("[Assistant tool call]: %s(%s)", name, args))
				}
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

// ============ 尾部预算选择(官方 select 移植) ============

type selectResult struct {
	head   []string // 摘要部分文本行
	recent []string // 保留的 recent 文本行
	split  int      // 原始消息的 split 索引(recent 从该索引起)
}

// selectRecent 从尾部往前累计 token 预算;放不下时把越界消息拆成 prefix(进 head)/suffix(进 recent)
func selectRecent(serialized []string, keepTokens int) *selectResult {
	if len(serialized) == 0 || keepTokens <= 0 {
		return nil
	}
	total := 0
	split := len(serialized)
	var splitPrefix, splitSuffix string
	for i := len(serialized) - 1; i >= 0; i-- {
		next := total + estimateText(serialized[i])
		if next > keepTokens {
			remaining := keepTokens - total
			if remaining > 0 {
				remainingChars := remaining * 4
				s := serialized[i]
				rs := []rune(s)
				if remainingChars <= 0 {
					split = i + 1
				} else if len(rs) > remainingChars {
					splitPrefix = string(rs[:len(rs)-remainingChars])
					splitSuffix = string(rs[len(rs)-remainingChars:])
				} else {
					splitSuffix = s
				}
				split = i + 1
			}
			break
		}
		total = next
		split = i
	}
	if split == 0 {
		return nil
	}
	head := make([]string, 0, split+1)
	head = append(head, serialized[:split]...)
	if splitPrefix != "" {
		head = append(head, splitPrefix)
	}
	recent := make([]string, 0, len(serialized)-split+1)
	if splitSuffix != "" {
		recent = append(recent, splitSuffix)
	}
	recent = append(recent, serialized[split:]...)
	return &selectResult{head: head, recent: recent, split: split}
}

// ============ 摘要提示词(官方 buildPrompt 移植) ============

func buildSummaryPrompt(previousSummary string, context []string) string {
	var prefix string
	if previousSummary != "" {
		prefix = "Update the anchored summary below using the conversation history above.\n" +
			"Preserve still-true details, remove stale details, and merge in the new facts.\n" +
			"<previous-summary>\n" + previousSummary + "\n</previous-summary>"
	} else {
		prefix = "Create a new anchored summary from the conversation history."
	}
	parts := append([]string{prefix, summaryTemplate}, context...)
	return strings.Join(parts, "\n\n")
}

// ============ 摘要生成 ============

// summaryTimeout 单次摘要生成的上界。
//
// 摘要是在请求处理路径内同步发起的上游调用。原来用的是 context.Background(),
// 既没有超时也不跟随客户端: 摘要模型一旦挂住(限流、地区拒绝、网络黑洞),
// 这个请求就会无限期挂着, 客户端断开也取消不掉。
const summaryTimeout = 90 * time.Second

func generateSummary(ctx context.Context, modelID, prompt string, maxSummary int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()

	body := map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens": maxSummary,
		"stream":     false,
	}
	resp, _, err := callZenAPI(ctx, body, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var raw map[string]any
	// 上游响应体封顶(2026-09-24 审计 P0-2: 出口是第三方节点, 响应字节不可信; 见 kit.DecodeJSONLimit)
	if err := kit.DecodeJSONLimit(resp.Body, &raw, kit.MaxUpstreamBodyBytes); err != nil {
		return "", err
	}
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			raw = d
		}
	}
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					return strings.TrimSpace(s), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no content in summary response")
}

// ============ 估算(官方 Token.estimate 近似: JSON 长度 / 4) ============

func estimateText(s string) int {
	return len([]rune(s)) / 4
}

func estimateJSON(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// ============ 会话状态 ============

func loadCompactState(sessionID string) *compactState {
	if !validCompactSessionID(sessionID) {
		return nil
	}
	compactStatesMu.Lock()
	defer compactStatesMu.Unlock()
	st := compactStates[sessionID]
	if st != nil {
		return st
	}
	st = &compactState{}
	compactStates[sessionID] = st
	evictCompactStatesLocked()
	return st
}

func updateCompactState(sessionID string, summary, recent string) {
	if !validCompactSessionID(sessionID) {
		return
	}
	compactStatesMu.Lock()
	defer compactStatesMu.Unlock()
	compactStates[sessionID] = &compactState{
		summary: summary,
		recent:  recent,
		updated: time.Now(),
	}
	evictCompactStatesLocked()
}

func cleanupCompactStates() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			compactStatesMu.Lock()
			cutoff := time.Now().Add(-24 * time.Hour)
			for k, v := range compactStates {
				if v.updated.Before(cutoff) {
					delete(compactStates, k)
				}
			}
			compactStatesMu.Unlock()
		case <-appRootCtx.Done():
			// 收到退出信号: 停止状态清理协程, 让进程能够真正停下。
			return
		}
	}
}

func requestSessionID(r map[string]any, hdr http.Header) string {
	// key 直接来自客户端可控的 header/body: 格式不合法一律当作"无 session"。
	if sid := hdr.Get("x-opencode-session"); validCompactSessionID(sid) {
		return sid
	}
	if sid := strField(r, "session_id"); validCompactSessionID(sid) {
		return sid
	}
	return ""
}

// ============ 压缩入口 ============

type compactOutcome struct {
	changed       bool
	note          string
	compactTokens int // 摘要生成消耗的估算 token
}

// maybeCompact 官方 compactIfNeeded 移植:
// 估算超 context - max(output, buffer) 时 -> select -> 摘要 -> 重组 [system]+[摘要]+recent
//
// ctx 贯穿到摘要生成: 压缩是请求处理路径内的同步上游调用, 必须能被客户端断开
// 取消, 否则一次挂住的摘要会把整个请求拖死。
func maybeCompact(ctx context.Context, params map[string]any, m *ZenModel, sessionID string) compactOutcome {
	cfg := getZenConfig()
	if !cfg.Enabled || !cfg.Compaction.Auto {
		return compactOutcome{}
	}
	context := m.Context
	if context <= 0 {
		return compactOutcome{}
	}
	output := m.Output
	buffer := cfg.Compaction.Buffer
	if buffer <= 0 {
		buffer = 20000
	}
	keep := cfg.Compaction.KeepTokens
	if keep <= 0 {
		keep = 8000
	}
	maxSum := cfg.Compaction.MaxSummary
	if maxSum <= 0 {
		maxSum = summaryOutputTokens
	}

	threshold := context - max(output, buffer)
	// 整请求只算一次: 阈值判定与下方 compact 日志共用同一份估算,
	// 不在请求路径上重复整包 marshal(P3 —— 不改 estimateJSON 定义)。
	est := estimateJSON(params)
	if est <= threshold {
		return compactOutcome{}
	}

	messages, _ := params["messages"].([]any)
	if len(messages) == 0 {
		return compactOutcome{}
	}

	// 1. 序列化(一一对应原始消息)
	serialized := make([]string, 0, len(messages))
	for _, msg := range messages {
		if mm, ok := msg.(map[string]any); ok {
			serialized = append(serialized, serializeMsg(mm))
		}
	}

	// 2. select 尾部预算
	sel := selectRecent(serialized, keep)
	if sel == nil || sel.split <= 0 {
		return compactOutcome{}
	}

	// 3. 增量摘要: 优先复用会话摘要; 无会话时检查是否有历史摘要消息
	st := loadCompactState(sessionID)
	previousSummary := ""
	previousRecent := ""
	if st != nil {
		previousSummary = st.summary
		previousRecent = st.recent
	}
	if previousSummary == "" {
		previousSummary = findExistingSummary(messages, sel.split)
	}

	head := strings.Join(sel.head, "\n\n")
	contextParts := []string{}
	if previousRecent != "" {
		contextParts = append(contextParts, previousRecent)
	}
	if head != "" {
		contextParts = append(contextParts, head)
	}
	if previousSummary == "" && head == "" && previousRecent == "" {
		return compactOutcome{}
	}
	prompt := buildSummaryPrompt(previousSummary, contextParts)

	// 4. 调摘要模型(默认同请求模型)
	summaryModel := cfg.Compaction.SummaryModel
	if summaryModel == "" {
		summaryModel = m.ID
	}
	log.Printf("  compact: ctx=%d est=%d > threshold=%d keep=%d split@%d summary_model=%s",
		context, est, threshold, keep, sel.split, summaryModel)
	summary, err := generateSummary(ctx, summaryModel, prompt, maxSum)
	if err != nil {
		log.Printf("  compact: summary generation failed (%v), falling back to truncation", err)
		return fallbackTruncate(params, m)
	}

	// 5. 重组: [原system]+[摘要]+[recent 尾部原始消息]
	var newMsgs []any
	for _, msg := range messages {
		if mm, ok := msg.(map[string]any); ok && strField(mm, "role") == "system" {
			newMsgs = append(newMsgs, mm)
		}
	}
	newMsgs = append(newMsgs, map[string]any{
		"role":    "system",
		"content": "[Conversation Summary]\n" + summary,
	})
	if st != nil && st.summary != "" {
		newMsgs = append(newMsgs, map[string]any{
			"role":    "system",
			"content": "[Previous Conversation Summary]\n" + st.summary,
		})
	}
	newMsgs = append(newMsgs, fixToolPairs(messages[sel.split:])...)

	updateCompactState(sessionID, summary, strings.Join(sel.recent, "\n\n"))
	params["messages"] = newMsgs
	return compactOutcome{
		changed:       true,
		note:          fmt.Sprintf("[compacted via summary] summary_model=%s kept=%d msgs", summaryModel, len(messages)-sel.split),
		compactTokens: estimateText(prompt) + estimateText(summary),
	}
}

// fixToolPairs 修复压缩/截断后断裂的 tool 调用配对。
//
// OpenAI 协议的硬性要求: role=tool 的消息必须是"对前一条 assistant 的 tool_calls
// 中某个 id 的回复"; 反过来, assistant 发起的每个 tool_call 也必须收到 tool 回复。
// 按 split 切片重组很容易把配对砍断(assistant 留下了、tool 回复被切掉, 或反之),
// 上游会直接 400 —— 这不是理论风险, 是压缩功能最常见的线上错误。
//
// 照抄 OmniRoute services/contextManager.ts:717-819（四遍扫描）。
//
// 逐字对照时必须保留的四处语义（每条都实证过, 不要"简化"回去）:
//
//  1. Pass 1 同时收集 `role=="tool"` 的 tool_call_id **和** Anthropic 形态
//     `user.content[].tool_result.tool_use_id`（:721-730）。只认前者的话,
//     Anthropic 形态的 tool_result 既不进"已声明"集合也不进"存活"集合 ——
//     既删不掉也留不对, 孤儿的会原样发给上游。
//  2. Pass 2 的 `!isLastMessage(idx)` 例外（:735-737）: **最后一条 assistant 的
//     tool_calls 永不修剪**。agent 循环里"刚发起调用、还没拿到结果"是正常瞬时
//     状态; 删掉它等于让 agent 的关键调用凭空蒸发 —— 客户端表现为
//     「没有任何错误、没有任何提示、任务直接中断」。
//  3. Pass 2 同时修剪 content 数组里的 `tool_use` 块（:751-760）。
//  4. 保留条件是 `!tc.id || toolResultIds.has(tc.id)` —— **没有 id 的 tool_call
//     一律保留**（:743）; 按空串查表会把它误删。
//
// Pass 4（:786-819）才做"修完后既无内容也无调用的 assistant 整条剔除",
// 必须放在最后 —— 否则会把上面那个"最后一条"例外又删回去。
func fixToolPairs(msgs []any) []any {
	if len(msgs) == 0 {
		return msgs
	}

	// --- Pass 1 (:718-731): 收集所有 tool 结果 id（两种形态）---
	toolResultIDs := map[string]bool{}
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if strField(m, "role") == "tool" {
			if id, _ := m["tool_call_id"].(string); id != "" {
				toolResultIDs[id] = true
			}
		}
		if strField(m, "role") == "user" {
			if blocks, ok := m["content"].([]any); ok {
				for _, bi := range blocks {
					b, ok := bi.(map[string]any)
					if !ok || strField(b, "type") != "tool_result" {
						continue
					}
					if id, _ := b["tool_use_id"].(string); id != "" {
						toolResultIDs[id] = true
					}
				}
			}
		}
	}

	// --- Pass 2 (:733-765): 修剪 assistant 里"未收到回复"的调用, 末条除外 ---
	filtered := make([]any, 0, len(msgs))
	for idx, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok || strField(m, "role") != "assistant" || idx == len(msgs)-1 {
			filtered = append(filtered, mi)
			continue
		}
		// :739 原版是 `const newMsg = { ...msg }` 浅拷贝, 仅在真有改动时替换;
		// 无改动时返回原对象(保持引用稳定)。
		modified := false
		newMsg := shallowCopyRecord(m)

		if tcs, ok := newMsg["tool_calls"].([]any); ok {
			live := make([]any, 0, len(tcs))
			for _, tc := range tcs {
				// :743 `!tc.id || toolResultIds.has(tc.id)` —— 无 id 保留
				tcm, _ := tc.(map[string]any)
				id, _ := tcm["id"].(string)
				if id == "" || toolResultIDs[id] {
					live = append(live, tc)
				}
			}
			if len(live) != len(tcs) {
				newMsg["tool_calls"] = live
				modified = true
			}
		}

		if blocks, ok := newMsg["content"].([]any); ok {
			live := make([]any, 0, len(blocks))
			for _, bi := range blocks {
				// :753-755 `block.type !== "tool_use" || !block.id || toolResultIds.has(block.id)`
				b, _ := bi.(map[string]any)
				if b == nil || strField(b, "type") != "tool_use" {
					live = append(live, bi)
					continue
				}
				id, _ := b["id"].(string)
				if id == "" || toolResultIDs[id] {
					live = append(live, bi)
				}
			}
			if len(live) != len(blocks) {
				newMsg["content"] = live
				modified = true
			}
		}

		if modified {
			filtered = append(filtered, newMsg)
		} else {
			filtered = append(filtered, mi)
		}
	}

	// --- Pass 3 (:767-784): 收集修剪后仍存活的 tool_call id（两种形态）---
	toolCallIDs := map[string]bool{}
	for _, mi := range filtered {
		m, ok := mi.(map[string]any)
		if !ok || strField(m, "role") != "assistant" {
			continue
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if tcm, ok := tc.(map[string]any); ok {
					if id, _ := tcm["id"].(string); id != "" {
						toolCallIDs[id] = true
					}
				}
			}
		}
		if blocks, ok := m["content"].([]any); ok {
			for _, bi := range blocks {
				if b, ok := bi.(map[string]any); ok &&
					strField(b, "type") == "tool_use" {
					if id, _ := b["id"].(string); id != "" {
						toolCallIDs[id] = true
					}
				}
			}
		}
	}

	// --- Pass 4 (:786-819): 丢掉孤儿结果, 丢掉修空的 assistant ---
	out := make([]any, 0, len(filtered))
	for _, mi := range filtered {
		m, ok := mi.(map[string]any)
		if !ok {
			out = append(out, mi)
			continue
		}
		role := strField(m, "role")

		if role == "tool" {
			if id, _ := m["tool_call_id"].(string); id != "" && !toolCallIDs[id] {
				continue // :790 孤儿 tool 结果整条丢弃
			}
		}

		if role == "user" {
			if blocks, ok := m["content"].([]any); ok {
				live := make([]any, 0, len(blocks))
				for _, bi := range blocks {
					// :796 `block.type !== "tool_result" || !block.tool_use_id || toolCallIds.has(...)`
					b, _ := bi.(map[string]any)
					if b == nil || strField(b, "type") != "tool_result" {
						live = append(live, bi)
						continue
					}
					id, _ := b["tool_use_id"].(string)
					if id == "" || toolCallIDs[id] {
						live = append(live, bi)
					}
				}
				if len(live) != len(blocks) {
					if len(live) == 0 {
						continue // :799 整个 user 消息只剩孤儿结果 -> 丢弃
					}
					m = shallowCopyRecord(m)
					m["content"] = live
				}
			}
		}

		if role == "assistant" {
			// :805-813 修完后既无内容也无调用 -> 整条丢弃
			if !contentEmpty(m) {
				out = append(out, m)
				continue
			}
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				out = append(out, m)
				continue
			}
			if blocks, ok := m["content"].([]any); ok && len(blocks) > 0 {
				out = append(out, m)
				continue
			}
			continue
		}

		out = append(out, m)
	}
	return out
}

// contentEmpty assistant 是否没有可见内容(只有空串/数组)。
func contentEmpty(m map[string]any) bool {
	switch c := m["content"].(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(c) == ""
	case []any:
		return len(c) == 0
	default:
		return false
	}
}

// findExistingSummary 从已有消息中寻找历史摘要(兼容有会话历史的客户端)
func findExistingSummary(messages []any, upTo int) string {
	if upTo < 0 {
		upTo = 0
	}
	if upTo > len(messages) {
		upTo = len(messages)
	}
	for i := 0; i < upTo; i++ {
		mm, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		c := strField(mm, "content")
		for _, prefix := range []string{"[Conversation Summary]", "[Previous Conversation Summary]"} {
			if strings.HasPrefix(c, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(c, prefix))
			}
		}
	}
	return ""
}

// fallbackTruncate 摘要失败时退回老式截断: 保留 system + 尾部消息至 60% 预算
func fallbackTruncate(params map[string]any, m *ZenModel) compactOutcome {
	messages, _ := params["messages"].([]any)
	if len(messages) == 0 {
		return compactOutcome{}
	}
	budget := int(float64(m.Context) * 0.6)
	type idxMsg struct {
		idx int
		msg any
	}
	kept := []idxMsg{}
	used := 0
	for i, msg := range messages {
		if mm, ok := msg.(map[string]any); ok && strField(mm, "role") == "system" {
			kept = append(kept, idxMsg{i, msg})
			used += estimateText(msgText(mm))
		}
	}
	for i := len(messages) - 1; i >= 0 && used < budget; i-- {
		isKept := false
		for _, k := range kept {
			if k.idx == i {
				isKept = true
				break
			}
		}
		if isKept {
			continue
		}
		mm, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		t := estimateText(msgText(mm))
		if used+t > budget {
			continue
		}
		kept = append(kept, idxMsg{i, messages[i]})
		used += t
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].idx < kept[b].idx })
	out := make([]any, 0, len(kept)+1)
	out = append(out, map[string]any{
		"role":    "system",
		"content": fmt.Sprintf("[context compaction] 上下文估算超过该模型限制(约 %d token),早期消息已被截断以继续会话。", m.Context),
	})
	for _, k := range kept {
		out = append(out, k.msg)
	}
	// 截断同样会砍断 tool 配对(keep 是按预算挑的, 不是按配对挑的), 统一修复。
	out = append(out[:1], fixToolPairs(out[1:])...)
	params["messages"] = out
	return compactOutcome{changed: true, note: "[compacted via truncation]"}
}
