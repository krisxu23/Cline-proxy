package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

// skipThoughtSignature Google 文档化的跳过哨兵: 历史未经过本进程时使用。
const skipThoughtSignature = "skip_thought_signature_validator"

// 注: thoughtSignatureCache 结构体与 newThoughtSignatureCache 声明在
// providers_config.go, 本文件只实现其方法, 不要重复声明类型。

func (c *thoughtSignatureCache) remember(id, sig string) {
	id = strings.TrimSpace(id)
	sig = strings.TrimSpace(sig)
	if id == "" || sig == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		for i, k := range c.order {
			if k == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.m[id] = sig
	c.order = append(c.order, id)
	for len(c.order) > c.max {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.m, old)
	}
}

func (c *thoughtSignatureCache) lookup(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[strings.TrimSpace(id)]
}

// readThoughtSignature 从工具调用中读取签名(兼容多种字段名)。
func readThoughtSignature(call map[string]any) string {
	if call == nil {
		return ""
	}
	extra, _ := call["extra_content"].(map[string]any)
	var google map[string]any
	if extra != nil {
		google, _ = extra["google"].(map[string]any)
	}
	for _, key := range []string{"thought_signature", "thoughtSignature"} {
		if google != nil {
			if v, ok := google[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		if v, ok := call[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeThoughtSignature 写入 extra_content.google.thought_signature。
func writeThoughtSignature(call map[string]any, sig string) {
	if call == nil {
		return
	}
	extra, _ := call["extra_content"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
		call["extra_content"] = extra
	}
	google, _ := extra["google"].(map[string]any)
	if google == nil {
		google = map[string]any{}
		extra["google"] = google
	}
	google["thought_signature"] = sig
}

// rememberSignaturesFromPayload 从非流式响应中提取并缓存签名。
func rememberSignaturesFromPayload(payload map[string]any, cache *thoughtSignatureCache) {
	if payload == nil || cache == nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		for _, key := range []string{"message", "delta"} {
			msg, _ := ch[key].(map[string]any)
			if msg == nil {
				continue
			}
			calls, _ := msg["tool_calls"].([]any)
			for _, cc := range calls {
				call, _ := cc.(map[string]any)
				if call == nil {
					continue
				}
				id, _ := call["id"].(string)
				if sig := readThoughtSignature(call); id != "" && sig != "" {
					cache.remember(id, sig)
				}
			}
		}
	}
}

// injectThoughtSignatures 为历史 assistant 消息中的工具调用回填签名。
func injectThoughtSignatures(body map[string]any, cache *thoughtSignatureCache) {
	if body == nil || cache == nil {
		return
	}
	messages, _ := body["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, cc := range calls {
			call, _ := cc.(map[string]any)
			if call == nil || readThoughtSignature(call) != "" {
				continue
			}
			sig := ""
			if id, _ := call["id"].(string); id != "" {
				sig = cache.lookup(id)
			}
			if sig == "" {
				sig = skipThoughtSignature
			}
			writeThoughtSignature(call, sig)
		}
	}
}

// markAllSignaturesSkipped 把历史工具调用的签名统一替换为跳过哨兵,
// 用于上游拒绝了缓存签名后的重放。
func markAllSignaturesSkipped(body map[string]any) {
	if body == nil {
		return
	}
	messages, _ := body["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, cc := range calls {
			call, _ := cc.(map[string]any)
			if call == nil {
				continue
			}
			writeThoughtSignature(call, skipThoughtSignature)
		}
	}
}

var missingSignatureRe = regexp.MustCompile(`(?i)thought[_ ]signature`)

// isMissingThoughtSignatureError 400 且 message 提到 thought_signature。
func isMissingThoughtSignatureError(status int, message string) bool {
	return status == 400 && missingSignatureRe.MatchString(message)
}

// providerIsGoogleGenerativeLanguage provider 是否指向 Google Gemini 原生端点。
// 端点判定只保留这一份, 供签名与代理两条路径共用, 避免两处副本各自漂移。
func providerIsGoogleGenerativeLanguage(p *modelProvider) bool {
	if p == nil {
		return false
	}
	cfg, ok := providerConfigFor(p.name)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(cfg.BaseURL), "generativelanguage.googleapis.com")
}

// providerNeedsThoughtSignatures 仅 Gemini(按名或 generativelanguage 域名)需要。
func providerNeedsThoughtSignatures(p *modelProvider) bool {
	if p == nil {
		return false
	}
	if p.name == "gemini" {
		return true
	}
	return providerIsGoogleGenerativeLanguage(p)
}

type sigSlot struct {
	id        string
	signature string
}

// signatureStreamExtractor 从 SSE 流中逐行提取工具调用签名。
type signatureStreamExtractor struct {
	cache   *thoughtSignatureCache
	pending map[int]*sigSlot
}

func newSignatureStreamExtractor(cache *thoughtSignatureCache) *signatureStreamExtractor {
	return &signatureStreamExtractor{cache: cache, pending: map[int]*sigSlot{}}
}

func (e *signatureStreamExtractor) ingestCall(call map[string]any, fallbackIndex int) {
	if call == nil {
		return
	}
	idx := fallbackIndex
	if i, ok := call["index"].(float64); ok {
		idx = int(i)
	}
	slot := e.pending[idx]
	if slot == nil {
		slot = &sigSlot{}
		e.pending[idx] = slot
	}
	if id, _ := call["id"].(string); id != "" {
		slot.id = id
	}
	if sig := readThoughtSignature(call); sig != "" {
		slot.signature = sig
	}
	if slot.id != "" && slot.signature != "" {
		e.cache.remember(slot.id, slot.signature)
	}
}

func (e *signatureStreamExtractor) ingestLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			msg, _ := ch[key].(map[string]any)
			if msg == nil {
				continue
			}
			calls, _ := msg["tool_calls"].([]any)
			for i, cc := range calls {
				call, _ := cc.(map[string]any)
				e.ingestCall(call, i)
			}
		}
	}
}

// push 接收一行或多行文本(调用方负责按 \n 切分)。
func (e *signatureStreamExtractor) push(text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			e.ingestLine(line)
		}
	}
}
