package app

import (
	"bufio"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	index   int
	id      string
	name    string
	args    string
	emitted bool
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

func loadOverrideContent() string {
	data, err := os.ReadFile("override.md")
	if err != nil {
		// override.md 是可选功能，文件不存在时静默使用客户端自带提示词
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	} else {
		log.Printf("  override.md is empty, using client system prompt")
	}
	return content
}

// extractStringContent 把 Anthropic 形态的 system 字段（字符串或 text 块数组）
// 拍平成一段纯文本。
//
// ★ 每一段都过 stripAnthropicBillingHeader（照抄 claude-to-openai.ts:16-19）。
//
//	参考实现在组装 OpenAI 请求时**逐条** system 入口都调它
//	（claude-to-openai.ts:145 / :147 / :155 / :158），不区分字符串形态还是块数组
//	形态。本函数是那两个分支在 Go 侧的唯一汇合点，故在此统一剥。
//
//	业务意义: Anthropic 会在部分 system prompt 顶部注入一行动态的
//	`x-anthropic-billing-header: <每请求都变的值>`。本网关的入站 Claude 路径正是
//	"Claude → OpenAI 再转发给非 Anthropic 上游"，不剥掉的话这一行会漏进上游
//	上下文；更糟的是它在 prompt 最开头且每请求轮换，会让整个前缀的 prompt-cache
//	永远不命中 —— 请求正常、回答正常，只有账单和延迟悄悄变差。
func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return stripAnthropicBillingHeader(s)
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, stripAnthropicBillingHeader(t))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// 照抄 OmniRoute executors/base.ts:970 与 executors/cliproxyapi.ts:347:
			// `if (Array.isArray(tb.tools)) tb.tools = sanitizeClaudeToolSchemas(tb.tools);`
			//
			// 参考实现原注释: "Sanitize invalid tool input_schemas (truncation
			// placeholders such as `enum: "[MaxDepth]"`, or index-keyed objects
			// where arrays are required) that Anthropic rejects with
			// `tools.N.custom.input_schema: JSON schema is invalid`"。
			//
			// 这里是与参考实现逐字对位的位置: input_schema 被原样搬进 OpenAI 的
			// parameters。若不先净化, Claude Code 之类客户端发来的截断占位符
			// (`enum: "[MaxDepth]"`) 或含 lookaround 的正则会原样上行并触发 400。
			schema := sanitizeClaudeToolSchema(tMap["input_schema"])
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  schema,
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		openAI["top_p"] = req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = req.ToolChoice
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		log.Printf("  system prompt: %d bytes (from override.md)", len(sysContent))
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []map[string]any

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						toolCallID, _ := b["tool_use_id"].(string)
						if toolCallID == "" {
							continue
						}
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      anthropicContentToString(b["content"]),
							"tool_call_id": toolCallID,
						})
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
				log.Printf("  anthropic req: assistant tool_calls=%d", len(toolCalls))
			} else if m.Role == "user" && len(toolResults) > 0 {
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
					content, _ := tr["content"].(string)
					id, _ := tr["tool_call_id"].(string)
					log.Printf("  anthropic req: tool_result id=%s content_len=%d prefix=%s", id, len(content), kit.Truncate(content, 400))
				}
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

// parseToolArgs 解析工具调用参数 JSON，带容错修复
func parseToolArgs(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	// 清理杂散引号前缀：上游流式输出偶发 "" 前缀（如 ""{"file_path":...}）
	for strings.HasPrefix(raw, `""`) {
		raw = strings.TrimPrefix(raw, `""`)
	}
	raw = strings.TrimSpace(raw)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if v == nil {
			return map[string]any{}, nil
		}
		return v, nil
	}
	// 整体被 JSON 字符串包裹（"{\"file_path\": ...}"）时，解包字符串后再解析
	if strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len(raw) >= 2 {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			var v2 any
			if json.Unmarshal([]byte(s), &v2) == nil && v2 != nil {
				return v2, nil
			}
		}
	}
	fixed := raw
	if strings.HasPrefix(fixed, "{") && !strings.HasSuffix(fixed, "}") {
		fixed += "}"
	} else if strings.HasPrefix(fixed, "[") && !strings.HasSuffix(fixed, "]") {
		fixed += "]"
	}
	if strings.HasSuffix(fixed, ",") {
		fixed = strings.TrimRight(fixed, ",") + "}"
	}
	if err := json.Unmarshal([]byte(fixed), &v); err == nil && v != nil {
		return v, nil
	}
	// 最终兜底：从杂散内容中提取首个 JSON 对象/数组
	if i := strings.IndexAny(raw, "{["); i >= 0 {
		openCh := raw[i]
		closeCh := byte('}')
		if openCh == '[' {
			closeCh = ']'
		}
		if j := strings.LastIndex(raw, string(closeCh)); j > i {
			sub := raw[i : j+1]
			if json.Unmarshal([]byte(sub), &v) == nil && v != nil {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid json: %s", kit.Truncate(raw, 120))
}

// extractToolSchemas 从 Anthropic 请求的 tools 定义中解析每个工具的 input_schema 属性集合，
// 用于转发 tool_use 时裁剪 input，避免多余字段触发客户端校验失败。
func extractToolSchemas(tools json.RawMessage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(tools) == 0 {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(tools, &arr); err != nil {
		return out
	}
	for _, t := range arr {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		var propNames []string
		for k := range props {
			propNames = append(propNames, k)
		}
		var required []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		log.Printf("  tool schema: name=%s properties=%v required=%v", name, propNames, required)
		if len(props) == 0 {
			continue
		}
		allowed := map[string]bool{}
		for k := range props {
			allowed[k] = true
		}
		out[name] = allowed
	}
	return out
}

// filterToolInput 将工具参数裁剪到客户端 schema 允许的字段内；
// 找不到 schema 或过滤后为空时保留原参数，避免丢参数。
func filterToolInput(name string, input map[string]any, schemas map[string]map[string]bool) map[string]any {
	allowed, ok := schemas[name]
	if !ok || len(allowed) == 0 {
		return input
	}
	out := map[string]any{}
	for k, v := range input {
		if allowed[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return input
	}
	return out
}

// anthropicContentToString 将 Anthropic content（字符串或块数组）转为纯文本
func anthropicContentToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		parts := []string{}
		for _, it := range arr {
			if b, ok := it.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	choices := getNested(openAI, "choices")
	if choices == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}

	text := ""
	choice0, ok := getNested(openAI, "choices", 0).(map[string]any)
	if !ok {
		out["content"] = []any{map[string]any{"type": "text", "text": text}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// reasoning_content -> thinking block (非流式路径, 与流式 thinking 透传一致)
	if msg != nil {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			contentBlocks = append([]any{map[string]any{"type": "thinking", "thinking": rc}}, contentBlocks...)
		}
	}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{}
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					if funcData == nil {
						continue
					}
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					if input == nil {
						input = map[string]any{}
					}
					id, _ := tcMap["id"].(string)
					if id == "" {
						id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), len(contentBlocks))
					}
					name, _ := funcData["name"].(string)
					if name == "" {
						continue
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	default:
		out["stop_reason"] = "end_turn"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	toolSchemas := extractToolSchemas(req.Tools)
	if len(toolSchemas) > 0 {
		log.Printf("  anthropic tools: %d schemas", len(toolSchemas))
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)
	req.Model = stripDisplayPrefix(req.Model)
	openAIReq["model"] = req.Model

	// system/developer 角色消息提升（照抄 OmniRoute
	// open-sse/handlers/chatCore/claudeSystemRole.ts）。
	//
	// 参考实现的调用点 (chatCore.ts:2305-2320) 在「Claude 语义透传」分支内:
	//
	//	if (isClaudeCodeSemanticPassthrough) {
	//	  if (provider !== "claude" || !shouldUseMidConversationSystem(body, model)) {
	//	    extractSystemRoleMessages(translatedBody);
	//	  } else {
	//	    relocateDirectiveOnlyMessages(translatedBody);
	//	  }
	//	}
	//
	// 对本网关而言入站**已经是** Anthropic Messages 形状（客户端直接发
	// /v1/messages），所以语义上就处在 isClaudeCodeSemanticPassthrough 那条路径上。
	//
	// ★ 作用域同样照抄: 本网关的 Anthropic 入站端点只服务 Claude 协议,
	//   不会把 Anthropic 形状的 body 交给任何非 Anthropic 上游 —— 即
	//   `provider !== "claude"` 在这个入口上恒为真，故恒走
	//   extractSystemRoleMessages 这一支（`mid-conversation-system` 是
	//   Anthropic 1M beta 档的例外, 见 providerSupportsMidConversationSystem）。
	//
	// 为什么这条必须做: Anthropic Messages API **拒绝** `system` / `developer`
	// 作为 messages[] 里的角色。Codex / OpenCode / Kilo Code 风格客户端会把
	// system 消息插在数组中间(尤其续接历史时), 参考实现记为
	// "Anthropic's Messages API rejects either as a chat role"。
	// 不提升的表现就是上游 400 —— 而 400 在 agent 客户端里常常只显示成
	// 任务无声中断, 没有任何可读提示。
	//
	// 客户端传来的 system 走的是 req.System(顶层字段), 与 messages[] 内的
	// system 角色是两件事: 前者由 anthropicToOpenAI 放在 msgs[0], 后者此前
	// **完全没人处理**, 会被当成 role:"system" 原样塞进中间位置。
	if msgs, ok := openAIReq["messages"].([]any); ok {
		hasSystemField := getNested(openAIReq, "messages", 0, "role") == "system"
		hasTools := false
		if tl, ok := openAIReq["tools"].([]any); ok && len(tl) > 0 {
			hasTools = true
		}
		if !providerSupportsMidConversationSystem(hasSystemField, hasTools, req.Model) {
			fixed, _, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
			if changed {
				openAIReq["messages"] = fixed
				log.Printf("  anthropic req: lifted system/developer roles out of messages[] (%d -> %d)", len(msgs), len(fixed))
			}
		}
	}

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	// 通用 Provider 直选: "provider:model"。
	//
	// 必须与 OpenAI 入口(/v1/chat/completions)保持一致: /v1/models 会把这些模型
	// 公开出去, 但此前只有 OpenAI 入口认这个前缀, 于是在 Anthropic 端点上
	// bai:glm-5.3 会落到 routeModel 被当成 cline 池的模型名送出去并必然失败 ——
	// 同一个模型名在三个协议端点上行为不一致。
	//
	// 直接复用候选链的调度器(单候选): 三种响应形状的转换只维护一份, 不为
	// 这一条路径再写一个新的 shape 转换分支。
	if name, sub, ok := parseProviderModel(req.Model); ok {
		setRouteHeader(w, name, req.Model, "")
		handleChainedChatAs(w, r, openAIReq,
			[]routeCandidate{{Upstream: name, Model: sub}}, req.Model,
			chainTarget{Shape: shapeAnthropic, ToolSchemas: toolSchemas})
		return
	}

	// 候选链: 路由别名(如 free-best)展开成有序候选, 逐站 failover。
	// openAIReq 已是转换后的 OpenAI 形状, 胜出那一站的响应再转回 Anthropic。
	if chain, matched, errMsg := resolveRouteChain(req.Model); matched {
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": errMsg, "type": "invalid_request_error"},
			})
			return
		}
		setRouteHeader(w, "chain", req.Model, "")
		handleChainedChatAs(w, r, openAIReq, chain, req.Model, chainTarget{Shape: shapeAnthropic, ToolSchemas: toolSchemas})
		return
	}

	// zen / clinepass 免费模型路由
	if route := routeModel(req.Model); route == "zen" {
		setRouteHeader(w, "zen", req.Model, "")
		handleZenAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	} else if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": zenRejectMessage(req.Model), "type": "invalid_request_error"},
		})
		return
	}

	// ClinePass 订阅池: cline-pass/ 前缀模型
	if strings.HasPrefix(strings.TrimSpace(req.Model), "cline-pass/") {
		setRouteHeader(w, "cline-pass", req.Model, "")
		handleClinePassAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	}

	activeCount := 0
	// 请求热路径: 必须走 poolSnapshot, 否则与 pickAccount 持 poolMu 改 a.Status 并发。
	p := poolSnapshot()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	// zen 免费模型落到 cline 池 = zen 熔断期间的路由降级
	if _, isZen := resolveZenFreeModel(req.Model); isZen {
		setRouteHeader(w, "cline", req.Model, "zen-degraded")
	} else {
		setRouteHeader(w, "cline", req.Model, "")
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"message": "No Cline accounts in pool. Add one in the admin panel (/admin/), or use *-free / cline-pass/* models.",
				"type":    "no_accounts_available",
			},
		})
		return
	}

	upstreamStream := req.Stream
	if !req.Stream && modelNeedsStream(normalizeRequestModel(req.Model)) {
		upstreamStream = true
		log.Printf("  anthropic model %s requires stream: forcing upstream stream, will aggregate", req.Model)
	}

	resp, acc, err := callClineAPIFailover(r.Context(), openAIReq, upstreamStream)
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()

	usageFn := accountUsageFn(acc, openAIReq)

	if req.Stream {
		handleAnthropicStreamWithUsage(w, resp, normalizeRequestModel(req.Model), toolSchemas, usageFn)
		return
	}

	if upstreamStream {
		out, err := collectStreamResponse(resp)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFn(u)
		}
		out = normalizeOpenAIResponse(out)
		anthropicResp := openAIToAnthropic(out)
		if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
			anthropicResp["stop_reason"] = "tool_use"
		}
		writeJSON(w, http.StatusOK, anthropicResp)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	out = normalizeOpenAIResponse(out)
	anthropicResp := openAIToAnthropic(out)

	if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}

	writeJSON(w, http.StatusOK, anthropicResp)
}

// handleZenAnthropic Anthropic Messages 请求路由到 zen 免费模型上游
func handleZenAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	zm, ok := resolveZenFreeModel(req.Model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", req.Model), "type": "invalid_request_error"},
		})
		return
	}
	isStream := req.Stream
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     upstreamZen,
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	sid := requestSessionID(openAIReq, r.Header)
	out := maybeCompact(r.Context(), openAIReq, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  anthropic zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(r.Context(), openAIReq, isStream)
	if err != nil {
		log.Printf("  anthropic zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		status := zenErrorStatus(err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, status)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		handleAnthropicStreamWithUsage(w, resp, zm.ID, toolSchemas, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusInternalServerError)
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	chatOut := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			chatOut = d
		}
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, resp.StatusCode)
}

func handleAnthropicStreamWithUsage(w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// 流式诊断日志改走共享、带轮转上限的 writeStreamLog(见其定义), 不再每请求独占句柄。
	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(d))
		w.Write([]byte(line))
		writeStreamLog(line)
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   modelName,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
			"stop_reason": nil,
		},
	})

	textIndex := new(int)
	*textIndex = -1
	hasText := false
	thinkingIndex := new(int)
	*thinkingIndex = -1
	hasThinking := false
	pendingTools := map[int]*toolAccumulator{}
	emitIndex := 0
	nextIndex := func() int {
		i := emitIndex
		emitIndex++
		return i
	}

	emitToolBlock := func(acc *toolAccumulator) {
		acc.emitted = true
		if acc.name == "" {
			log.Printf("  tool_use missing name, skipping (id=%s)", acc.id)
			return
		}
		idx := nextIndex()
		id := acc.id
		if id == "" {
			id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), idx)
			log.Printf("  tool_use missing id, generated %s", id)
		}
		argsObj, err := parseToolArgs(acc.args)
		if err != nil {
			log.Printf("  tool args parse failed for %s: %v (raw: %s)", acc.name, err, kit.Truncate(acc.args, 300))
			argsObj = map[string]any{}
		}
		if inputMap, ok := argsObj.(map[string]any); ok {
			argsObj = filterToolInput(acc.name, inputMap, toolSchemas)
		}
		parsed, _ := json.Marshal(argsObj)
		// 照抄 OmniRoute open-sse/translator/helpers/toolCallShim.ts:112 `applyToolCallShimToBuffer`。
		//
		// 参考实现原注释（逐字）:
		//
		//	// Applied on the assembled OpenAI tool-call arguments after streaming, just
		//	// before they are re-emitted as a single Claude input_json_delta.
		//
		// 本处正是那个消费点: acc.args 已由流式分片拼装完毕(OpenAI 形态的
		// tool_calls[].function.arguments), 此刻把它作为**单条** Claude
		// input_json_delta 发给客户端。
		//
		// 为什么必须在"发出去之前"清洗（而非上游侧）: Claude Code 的 Read 工具
		// 对 limit>2000 / 负数 offset / 非 PDF 带 pages 会**直接拒收并重试**,
		// 每轮重发整个上下文 —— token 成倍烧掉且表现为任务无声中断。
		// 无 shim 时 applyToolCallShimToBuffer 原样返回 raw(不做 JSON 往返),
		// 故对不认识的工具零影响。
		if hasToolCallShim(acc.name) {
			cleaned := applyToolCallShimToBuffer(acc.name, string(parsed))
			log.Printf("  tool_call_shim applied: name=%s before=%s after=%s",
				acc.name, kit.Truncate(string(parsed), 300), kit.Truncate(cleaned, 300))
			parsed = []byte(cleaned)
		}
		log.Printf("  tool_use emit: name=%s id=%s input=%s", acc.name, id, string(parsed))
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(parsed),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	}

	processSSELine := func(line string) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			return
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			return
		}
		if onUsage != nil {
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				onUsage(u)
			}
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}

		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			return
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				*textIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		// reasoning_content -> thinking block (reasoning 透传,
		// borrowed from hayou2002/clinepass-proxy: CherryStudio 等客户端
		// 依赖 thinking 块显示思考过程).
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			if !hasThinking {
				hasThinking = true
				*thinkingIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *thinkingIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *thinkingIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": rc,
				},
			})
		}

		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{index: idx}
					pendingTools[idx] = acc
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					} else if argsRaw, ok := fn["arguments"]; ok && argsRaw != nil {
						if bts, err := json.Marshal(argsRaw); err == nil {
							acc.args = string(bts)
						}
					}
				}
			}
		}

		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	reader := bufio.NewReader(upstream.Body)

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			processSSELine(line)
		}
		if err != nil {
			break
		}
	}

	// Stop text block if active
	if hasText {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *textIndex,
		})
	}

	// Stop thinking block if active
	if hasThinking {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *thinkingIndex,
		})
	}

	// Emit any remaining un-emitted tool blocks
	for _, acc := range pendingTools {
		if !acc.emitted {
			emitToolBlock(acc)
		}
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 0,
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}
