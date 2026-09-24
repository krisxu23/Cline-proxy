package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"free-router/internal/kit"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
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
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []anthropicMsg  `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
	// P1-7: 温度字段用 any 判存在性 —— float64 上「未传」与「显式 0」同为零值,
	// 旧写法 `!= 0` 会把客户端显式传的 temperature:0 / top_p:0(贪心解码, agent
	// 工具常用)当缺省丢掉, 上游按默认温度跑。JSON 解出非 nil 即客户端传过(含 0),
	// 原样透传; 未传为 nil, 维持旧行为不注入。
	Temperature any             `json:"temperature,omitempty"`
	TopP        any             `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	// 照抄 claude-to-openai.ts:252-271 —— Claude 侧两种推理强度方言：
	//   - `thinking.budget_tokens`（Claude 原生客户端）
	//   - `output_config.effort`（Claude Code 客户端，严格优先）
	// 两者都需解出来才能映射成 OpenAI 的 `reasoning_effort`。
	Thinking     json.RawMessage `json:"thinking,omitempty"`
	OutputConfig json.RawMessage `json:"output_config,omitempty"`
	Extra        map[string]any  `json:"-"`
}

// overrideCache 缓存 override.md 的读取结果:loadOverrideContent 在每个请求的
// 热路径上被调(anthropicToOpenAI 与 applyOverride 各一), 旧实现每请求
// os.ReadFile + 两条日志。现在 stat 判 mtime, 未变不重读;值未变不打日志。
// 带互斥 —— 服务长驻并发。
var overrideCache struct {
	mu      sync.Mutex
	modTime time.Time
	content string
	loaded  bool
}

func loadOverrideContent() string {
	overrideCache.mu.Lock()
	defer overrideCache.mu.Unlock()
	// override.md 是可选功能，文件不存在时静默使用客户端自带提示词。
	// stat 不存在时同时让缓存失效, 防止文件被删/工作目录切换后返回陈旧值。
	fi, err := os.Stat("override.md")
	if err != nil {
		overrideCache.loaded = false
		overrideCache.content = ""
		return ""
	}
	mod := fi.ModTime()
	if !overrideCache.loaded || overrideCache.modTime != mod {
		data, err := os.ReadFile("override.md")
		if err != nil {
			// 刚被并发删除: 沿用上次已知值(stat 成功说明此刻还在, 概率极低)。
			return overrideCache.content
		}
		content := strings.TrimSpace(string(data))
		if !overrideCache.loaded || content != overrideCache.content {
			// 只在值变化时打日志(首次加载也算变化)。
			if content != "" {
				log.Printf("  using override.md as system prompt (%d bytes)", len(content))
			} else {
				log.Printf("  override.md is empty, using client system prompt")
			}
		}
		overrideCache.modTime = mod
		overrideCache.content = content
		overrideCache.loaded = true
	}
	return overrideCache.content
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
			//
			// ★ 第二层照抄 claude-to-openai.ts:232 `parameters: normalizeToolSchema(record.input_schema)`:
			// 参考实现是**两步** —— executor 先 sanitize（剥非法构造），translator
			// 再 normalize（给 `{"type":"object"}` 补 `properties: {}`）。OpenAI 的
			// strict 校验要求 object schema 必带 `properties`，Anthropic/MCP 工具常省略
			// （#1898），只 sanitize 不 normalize 仍会 400。顺序不可反：先剥再补，
			// 否则剥空 schema 后 normalize 会得到无意义的空 properties。
			schema := normalizeToolSchema(sanitizeClaudeToolSchema(tMap["input_schema"]))
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
	// P1-7: 只有客户端显式传过 temperature/top_p 才透传 —— 显式 0(贪心解码)
	// 必须原样下发, 不能再被 `!= 0` 判成缺省而丢弃。
	if req.Temperature != nil {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		openAI["top_p"] = req.TopP
	}
	// P1-8: stop_sequences 此前声明但从未转发, 停止词静默失效。
	// 解析为字符串数组, 非空才写入 OpenAI 的 stop; 解析失败或空数组直接忽略, 不 panic。
	if req.Stop != nil {
		var stops []string
		if json.Unmarshal(req.Stop, &stops) == nil && len(stops) > 0 {
			openAI["stop"] = stops
		}
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		// 照抄 claude-to-openai.ts:244-248 —— Claude 的 tool_choice 是对象
		// 形态（{type:"auto"} / {type:"any"} / {type:"tool",name:...}），
		// OpenAI 是字符串形态（"auto" / "required"）或对象形态
		// （{type:"function",function:{name:...}}）。必须翻译，直接透传
		// 会让上游收到一个它不认识的字段结构。
		var tc any
		if json.Unmarshal(req.ToolChoice, &tc) == nil {
			openAI["tool_choice"] = convertToolChoice(tc)
		} else {
			// 解析失败对位参考实现的 `if (!choice) return "auto"`。
			openAI["tool_choice"] = "auto"
		}
	}

	// 照抄 claude-to-openai.ts:251-270 —— 把 Claude 侧的 thinking 控制翻译成
	// OpenAI 的 `reasoning_effort`。
	//
	// 参考实现原注释（逐字）:
	//
	//	// Reasoning effort: map Claude-side thinking controls to OpenAI reasoning_effort.
	//	// Priority: output_config.effort (Claude Code) > thinking.budget_tokens (Claude native).
	//	// Budget buckets match the reverse mapping in thinkingBudget.ts::setCustomBudget.
	//
	// Claude Code 客户端用 `output_config.effort` 表达推理强度，Claude 原生客户端用
	// `thinking.budget_tokens`。不翻译则上游收到一个它不认识的字段（被静默忽略或
	// 直接 400），模型实际跑在默认强度上 —— 用户以为自己调过档，实际没有。
	//
	// 顺序照抄参考实现：tools → tool_choice → reasoning_effort。
	if req.Thinking != nil || req.OutputConfig != nil {
		body := map[string]any{}
		if req.Thinking != nil {
			var th map[string]any
			if json.Unmarshal(req.Thinking, &th) == nil {
				body["thinking"] = th
			}
		}
		if req.OutputConfig != nil {
			var oc map[string]any
			if json.Unmarshal(req.OutputConfig, &oc) == nil {
				body["output_config"] = oc
			}
		}
		if e := openAIReasoningEffort(body); e != "" {
			openAI["reasoning_effort"] = e
		}
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
			// P2: image 块此前被无条件跳过 —— 视觉输入静默消失, 纯图回合还会
			// 产出空 content。先收集可渲染 URL(有图时才在下方按原序组装分段)。
			var imageURLs []string

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						if kind, a, d := anthropicImageSource(b); kind != "" {
							imageURLs = append(imageURLs, anthropicImageDataURL(kind, a, d))
						}
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
				// P3: 每条 tool_result 打 400B content 前缀的日志已删 —— 热路径
				// 最多 (400B × 结果数)/轮, 纯观测噪声。
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
				}
				// P1-9: 工具结果之后的用户文字指示不能丢 —— 照 openai_anthropic.go
				// anthropicUserToOpenAI 的正确做法, 在 tool 消息之后补一条 user 消息。
				// 同回合的 image 块也不能丢(带图的工具轮): 有图时按原序组装全量分段。
				if len(imageURLs) > 0 {
					msgs = append(msgs, map[string]any{
						"role":    "user",
						"content": anthropicBlocksToOpenAIContent(c, imageURLs),
					})
				} else if text := strings.Join(textParts, "\n"); text != "" {
					msgs = append(msgs, map[string]any{"role": "user", "content": text})
				}
			} else if len(imageURLs) > 0 {
				// 有图的回合按原序组装 text/image_url 分段(纯图回合也有非空 content);
				// 不支持图的上游由 providers_chat 的 stripTypesForModel/model-strip 剥,
				// 不在协议转换层无条件丢视觉输入。
				msgs = append(msgs, map[string]any{
					"role":    m.Role,
					"content": anthropicBlocksToOpenAIContent(c, imageURLs),
				})
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		default:
			// P1-10: content 为 null / 非 string 非 []any 形态时不能整条静默丢弃 ——
			// 对话历史缺轮次会破坏 tool_use/tool_result 配对, 上游直接 400。
			// anthropicContentToString 对 nil 安全; 转出空字符串时才跳过该消息。
			if text := anthropicContentToString(c); text != "" {
				msgs = append(msgs, map[string]any{"role": m.Role, "content": text})
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
		// P3: 此前每工具每请求打一行 schema 日志(Claude Code ~20 工具 = 20 行/请求),
		// 还为此先构建 propNames 切片 —— 纯热路径噪声与多余分配, 一并删除。
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

// anthropicImageSource 解开 image 块的 source, 返回构成可渲染 URL 的分量:
// "base64" → (media_type, data); "url" → ("", url); 无有效图 → kind 为空。
// 拆开返回是为了让"有效性判定"零分配(主循环每块都判), 拼接只对有效图做一次。
func anthropicImageSource(b map[string]any) (kind, a, d string) {
	src, _ := b["source"].(map[string]any)
	if src == nil {
		return "", "", ""
	}
	switch src["type"] {
	case "base64":
		data, _ := src["data"].(string)
		if data == "" {
			return "", "", ""
		}
		mt, _ := src["media_type"].(string)
		return "base64", mt, data
	case "url":
		u, _ := src["url"].(string)
		if u == "" {
			return "", "", ""
		}
		return "url", "", u
	}
	return "", "", ""
}

// anthropicImageDataURL 组成 OpenAI image_url 的 url 字段:
// base64 → `data:<mime>;base64,<data>`;url 源原样透传。
// Anthropic 协议必带 media_type, 缺失时按 image/png 兜底而非丢图。
func anthropicImageDataURL(kind, a, d string) string {
	if kind == "url" {
		return d
	}
	mt := a
	if mt == "" {
		mt = "image/png"
	}
	return "data:" + mt + ";base64," + d
}

// anthropicBlocksToOpenAIContent 把 Anthropic content 块按**原序**组装成 OpenAI
// content 数组(text → {"type":"text"}, image → {"type":"image_url"})。
// imageURLs 是主循环按同一谓词(anthropicImageSource != 空)收集的队列, 此处
// 同谓词跳过无效图, 队列与块严格对齐。
func anthropicBlocksToOpenAIContent(blocks []any, imageURLs []string) []any {
	parts := make([]any, 0, len(blocks))
	img := 0
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			if kind, _, _ := anthropicImageSource(b); kind != "" && img < len(imageURLs) {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURLs[img]},
				})
				img++
			}
		}
	}
	return parts
}

// cleanToolUseInput 是流式 emitToolBlock 与非流式 openAIToAnthropicWithMap
// **共用**的"裁剪 + tool_call_shim"清洗(此前只有流式过这两道, 非流式与聚合
// 路径完全裸奔 —— Claude Code 的 Read 对 limit>2000 直接拒收并每轮重发整个
// 上下文, token 成倍烧掉)。
//
// 两个名字各查各的表:
//   - clientName(restoreClaudeToolName 恢复后的**客户端声明名**): toolSchemas
//     由客户端原始 tools 构建, 键就是客户端名 —— 用上游回显名查会 miss, 把
//     未裁剪字段直发严格校验的客户端。miss 时回退 echoName(客户端声明的恰是
//     canonical 名、恢复走了 REVERSE_MAP 反向时, 键在 echo 侧)。
//   - shim 表两种键都有(Claude 权威名 Read / 客户端原名 submit_pr_review),
//     先按 clientName 查, 未命中回退 echoName。
//
// 顺序照抄参考实现: 先裁字段(filterToolInput), 再套结构 shim。
// 返回清洗后的**值**: 流式调用方自行 marshal 成 input_json_delta 的
// partial_json, 非流式直接放进 tool_use.input(此前非流式不过这两道清洗)。
func cleanToolUseInput(echoName, clientName string, input any, schemas map[string]map[string]bool) any {
	filtered := input
	if m, ok := input.(map[string]any); ok {
		key := clientName
		if _, ok := schemas[key]; !ok {
			if _, ok := schemas[echoName]; ok {
				key = echoName
			}
		}
		filtered = filterToolInput(key, m, schemas)
	}
	shimName := clientName
	if !hasToolCallShim(shimName) {
		shimName = echoName
	}
	// 值级应用 shim(与 applyToolCallShimToBuffer 同一 resolve, 省一次字符串往返):
	// 无 shim 原样返回; shim 只认对象, 数组/标量按其语义原样穿过。
	if fn, ok := resolveToolCallShim(shimName); ok {
		before, _ := json.Marshal(filtered)
		patched := fn(filtered)
		after, _ := json.Marshal(patched)
		log.Printf("  tool_call_shim applied: name=%s before=%s after=%s",
			shimName, kit.Truncate(string(before), 300), kit.Truncate(string(after), 300))
		filtered = patched
	}
	return filtered
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
	return openAIToAnthropicWithMap(openAI, nil, nil)
}

// openAIToAnthropicWithMap 是 openAIToAnthropic 的带映射版本。
//
// ★ 对应参考实现 `convertOpenAINonStreamingToClaude(openaiResponse, toolNameMap?)`
//
//	  的**可选尾参**（handlers/responseTranslator.ts:684-690）。参考实现在
//
//		name: restoreClaudeToolName(toString(fn.name), toolNameMap ?? null)
//
//	  （:740）处还原工具名，注释（逐字）:
//
//		`toolNameMap` carries request-side aliases; when it does not resolve a name,
//		`restoreClaudeToolName` upgrades known Claude Code tools to their canonical
//		PascalCase ("bash" → "Bash", "croncreate" → "CronCreate"). Without this, a
//		non-streaming upstream JSON body (or a stream:true request the upstream
//		answered with application/json) reaches Claude Code with lowercase tool_use
//		names the CLI rejects as "No such tool available".
//
// 为什么必须有这一步: 请求侧把 `read_file` 伪装成 `Read` 发出（见
// providers_chat.go 的 cloak 接入点），上游就会**回显 `Read`**。若不在回程还原，
// 客户端收到的是它从未声明过的工具名 → Claude Code 报
// "No such tool available: Read"（它声明的叫 read_file）。
//
// nameMap 为 nil 时行为与 openAIToAnthropic 完全一致（纯 PascalCase 真 Claude Code
// 流量不走伪装，故恒为 nil）—— 保证对既有路径零影响。
//
// toolSchemas 为请求侧 extractToolSchemas 的产物, 传入即让非流式 tool_use 也过
// "裁剪 + shim" 清洗(与流式 emitToolBlock 共用 cleanToolUseInput); nil 表示不做
// 裁剪(shim 仍生效)。
func openAIToAnthropicWithMap(openAI map[string]any, nameMap *toolNameMap, toolSchemas map[string]map[string]bool) map[string]any {
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
	toolUseEmitted := false
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			// 重建时**保留**已前插的 thinking 块(顺序 thinking → text → tool_use):
			// 此前 `contentBlocks = []any{}` 整体重建, reasoning+tool_calls 共存的
			// 推理模型回合会把刚前插的 thinking 静默删掉(与流式语义漂移, 原推理
			// 内容只能靠占位符兜底, 永久丢失)。
			blocks := []any{}
			for _, b := range contentBlocks {
				if bm, ok := b.(map[string]any); ok && bm["type"] == "thinking" {
					blocks = append(blocks, b)
				}
			}
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				tcMap, ok := tcItem.(map[string]any)
				if !ok {
					continue
				}
				funcData, _ := tcMap["function"].(map[string]any)
				if funcData == nil {
					continue
				}
				id, _ := tcMap["id"].(string)
				if id == "" {
					id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), len(blocks))
				}
				name, _ := funcData["name"].(string)
				if name == "" {
					continue
				}
				echoName := name
				// 工具名还原（照抄 responseTranslator.ts:740
				// `restoreClaudeToolName(toString(fn.name), toolNameMap ?? null)`）。
				//
				// nameMap 为空时 restoreClaudeToolName 仍会走它的
				// "canonical casing upgrade" 分支（`bash` → `Bash`），
				// 这正是参考实现想要的：非流式上游 JSON（或 stream:true 但上游
				// 以 application/json 回）送到 Claude Code 时，小写工具名会被
				// CLI 拒为 "No such tool available"。
				name = restoreClaudeToolName(name, nameMap)
				// input: OpenAI arguments 是 JSON 字符串, Anthropic 要求对象。
				// 坏 JSON(截断的半截参数)走 parseToolArgs 同款容错, 救不回来兜底 {} ——
				// 此前 unmarshal 失败会把**字符串**原样透传成 input(协议非法, 客户端
				// 按完整调用执行后必失败, 也无从按 max_tokens 语义重试)。
				input := funcData["arguments"]
				if argsStr, ok := input.(string); ok {
					parsedArgs, perr := parseToolArgs(argsStr)
					if perr != nil {
						log.Printf("  tool args parse failed for %s: %v (raw: %s)",
							name, perr, kit.Truncate(argsStr, 300))
						parsedArgs = map[string]any{}
					}
					input = parsedArgs
				}
				if input == nil {
					input = map[string]any{}
				}
				// 裁剪 + shim: 与流式 emitToolBlock 共用 cleanToolUseInput ——
				// 此前非流式(含聚合)完全不过这两道清洗, 裸奔直发客户端。
				input = cleanToolUseInput(echoName, name, input, toolSchemas)
				block := map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": input,
				}
				blocks = append(blocks, block)
				toolUseEmitted = true
			}
			contentBlocks = blocks
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
	// 覆写守卫: 只有映射结果仍是 end_turn 且确有 tool_calls 时才覆写成 tool_use。
	// finish_reason=length 映射出的 max_tokens 必须保留 —— 截断的半截参数伪装成
	// 完整调用会让客户端解析失败且无从重试(流式路径此前就是这个正确行为)。
	if toolUseEmitted && out["stop_reason"] == "end_turn" {
		out["stop_reason"] = "tool_use"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
			// 缓存 token 是 Anthropic 的**合法字段**, 客户端靠它算缓存命中率 ——
			// 原实现整块丢掉(2026-09-24 审查)。OpenAI 侧把命中量放在
			// prompt_tokens_details.cached_tokens, 写入量是平铺的
			// cache_creation_input_tokens。
			// 注意 total_tokens **不该**映射: Anthropic 没有这个字段。
			if d, ok := um["prompt_tokens_details"].(map[string]any); ok {
				if v, has := d["cached_tokens"]; has {
					usage["cache_read_input_tokens"] = v
				}
			}
			if v, has := um["cache_creation_input_tokens"]; has {
				usage["cache_creation_input_tokens"] = v
			}
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
	//   `provider !== "claude"` 在这个入口上恒为真，故主走
	//   extractSystemRoleMessages 这一支（`mid-conversation-system` 是
	//   Anthropic 1M beta 档的例外, 走 else 的 relocateDirectiveOnlyMessages,
	//   见 providerSupportsMidConversationSystem）。
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
			// P0: 第 2 返回值此前被 `_` 丢弃 —— 提升把(含 msgs[0] 顶层 system 的)
			// 全部 system 内容从 messages 里拿走却无人回填, 上游一个字都收不到。
			// changed 时把 sysOut 写回请求顶层 system(参考实现写入 payload.system;
			// 探针 G3 锁定的最终形态: system 只用顶层字段承载)。
			fixed, sysOut, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
			if changed {
				openAIReq["messages"] = fixed
				if sysOut != nil {
					openAIReq["system"] = sysOut
				}
				log.Printf("  anthropic req: lifted system/developer roles out of messages[] (%d -> %d)", len(msgs), len(fixed))
			}
		} else {
			// 参考实现 else 分支(claudeSystemRole.ts:167-247, 1M-context Opus 档):
			// mid-conversation-system 通路**故意**保留 system 角色, 但 directive-only
			// 形态(空 content + output_config, Claude Code 客户端的形态)停在
			// messages[0] 会被 Anthropic 当作初始 system 位置拒收 400 —— 移到
			// 首个真实轮次之后。
			fixed, newOC, changed := relocateDirectiveOnlyMessages(msgs, nil, false)
			if changed {
				openAIReq["messages"] = fixed
				if newOC != nil {
					openAIReq["output_config"] = newOC
				}
				log.Printf("  anthropic req: relocated directive-only system message off messages[0]")
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
		// P2: 守卫同非流式路径 —— 仅 end_turn 可覆写成 tool_use, 保留 max_tokens。
		if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 &&
			anthropicResp["stop_reason"] == "end_turn" {
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
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	// P1: usage 必须在解 data 包裹**之后**再取(与 proxy_stream.go 同序) ——
	// 旧顺序先查外层 raw["usage"], 包裹形态的 usage 永远取不到, 账单/统计漏记。
	if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	out = normalizeOpenAIResponse(out)
	anthropicResp := openAIToAnthropic(out)

	// P2: 仅当映射结果仍是 end_turn 且 tool_calls 非空才覆写成 tool_use ——
	// finish_reason=length 映射出的 max_tokens 必须保留(与 openAIToAnthropic
	// 内的覆写守卫同口径), 截断的半截参数不得伪装成完整调用。
	if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 &&
		anthropicResp["stop_reason"] == "end_turn" {
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
	chatOut := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			chatOut = d
		}
	}
	// P1: 同 handleAnthropicMessages 非流式路径 —— 先解 data 包裹再取 usage。
	if u, ok := chatOut["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	anthropicResp := openAIToAnthropic(chatOut)
	// P2: 守卫同非流式路径 —— 仅 end_turn 可覆写成 tool_use, 保留 max_tokens。
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 &&
		anthropicResp["stop_reason"] == "end_turn" {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, resp.StatusCode)
}

// handleAnthropicStreamWithUsage 是带可选工具名映射的流式 Claude-shape 回写入口。
//
// toolNameMap 对位参考实现 `utils/stream.ts:restoreClaudePassthroughToolUseName`
// 的映射来源（chatCore.ts:2592-2610 从 translatedBody._toolNameMap 取出后
// 一路透传到流式转换器）。为 nil 时行为与无映射版本完全一致。
func handleAnthropicStreamWithUsage(w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	handleAnthropicStreamWithToolNameMap(w, upstream, modelName, toolSchemas, onUsage, nil)
}

// handleAnthropicStreamWithToolNameMap 是实际实现。
//
// ★ 还原位置的出处: emitToolBlock 内 `content_block.name` 是**上游回显**的工具名。
//
//	请求侧 cloak 后上游回的是别名（如 `Read`），必须在写给 Claude Code 之前
//	还原成客户端声明的原名（如 `read_file`），否则 CLI 报
//	"No such tool available: Read"。
func handleAnthropicStreamWithToolNameMap(w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any), toolNameMap *toolNameMap) {
	log.Printf("  anthropic stream: starting real-time forward")
	// P2: Flush 断言必须挪到 WriteHeader **之前** —— 旧顺序先提交 200+SSE 头,
	// 再因 ResponseWriter 不支持 Flush 直接 return: 客户端拿到空 200 流, 既无
	// 任何事件, 也永远到不了下方的空流守卫(静默挂起)。此处尚未提交, 显式 500。
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": "response writer does not support flushing", "type": "api_error"},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	// P3: take-last 暂存末次上游 usage —— message_start / message_delta 的计量
	// 回填与 onUsage(内部 tracker)共用同一份。
	var lastUsage map[string]any
	usageInt := func(key string) int {
		if v, ok := lastUsage[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	// P3: message_start 改为**惰性首发**(任何真实事件写出之前补发)。上游常见
	// "usage 帧先行、正文在后"的形态(见 stream_firstline_test 首帧带 usage),
	// 在流打开瞬间就发只会把 input_tokens 恒写成 0; 惰性化后 usage 先到即可
	// 回填, 且 message_start 先于一切事件的协议序不被破坏。
	msSent := false
	var emit func(event string, data any)
	emitMessageStart := func() {
		if msSent {
			return
		}
		msSent = true
		emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   modelName,
				"usage": map[string]any{
					"input_tokens":  usageInt("prompt_tokens"),
					"output_tokens": usageInt("completion_tokens"),
				},
				"stop_reason": nil,
			},
		})
	}
	// 流式诊断日志改走共享、带轮转上限的 writeStreamLog(见其定义), 不再每请求独占句柄。
	emit = func(event string, data any) {
		if !msSent {
			emitMessageStart()
		}
		d, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(d))
		w.Write([]byte(line))
		writeStreamLog(line)
		flusher.Flush()
	}

	textIndex := new(int)
	*textIndex = -1
	hasText := false
	thinkingIndex := new(int)
	*thinkingIndex = -1
	hasThinking := false
	// reasoningDelivered 与 hasThinking 的区别: hasThinking 管"要不要开/关
	// thinking 块", reasoningDelivered 管"用户到底看到了东西没有"。
	// 只发空白的 reasoning_content 会开出一个空的 thinking 块 —— 对用户等于零产出,
	// 因此交付判定用 trim 后的口径(与 stream_delivery.go 的流级判据一致)。
	reasoningDelivered := false
	pendingTools := map[int]*toolAccumulator{}
	// P3: 上游 SSE 已发过 error 帧(processSSELine 内置位) —— error 即终止,
	// 收尾的空流守卫与 message_delta/message_stop 据此整体跳过, 不补第二条 error。
	upstreamErrored := false
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
		// 工具名恢复(照抄 OmniRoute utils/stream.ts:restoreClaudePassthroughToolUseName,
		// 映射来源 chatCore.ts:2592-2610; 非流式对位 responseTranslator.ts:740):
		// 产出**写给客户端的原名**(如 read_file), 不碰 acc.name(上游回显别名)。
		// P2: 必须在 filterToolInput **之前**算好 —— toolSchemas 键=客户端声明名,
		// 只用回显名查会在名字不一致时 miss, 未裁剪字段直达严格校验的客户端。
		emitName := restoreClaudeToolName(acc.name, toolNameMap)
		if inputMap, ok := argsObj.(map[string]any); ok {
			// 与 cleanToolUseInput 同键序: 先按客户端声明名查, miss 再回退回显名。
			filterKey := emitName
			if _, ok := toolSchemas[filterKey]; !ok {
				if _, ok := toolSchemas[acc.name]; ok {
					filterKey = acc.name
				}
			}
			argsObj = filterToolInput(filterKey, inputMap, toolSchemas)
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
		// emitName(客户端声明原名, 如 read_file)已在 filter 前算好 —— 这里只把它
		// 写进发给客户端的 content_block.name; acc.name(上游回显别名, 如 Read)
		// 本身不改, shim 仍按 acc.name 查表(shim 表按 Claude 权威名建索引)。
		log.Printf("  tool_use emit: name=%s id=%s input=%s", emitName, id, string(parsed))
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  emitName,
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
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}
		// P1: usage 必须在解 data 包裹**之后**再取(与 proxy_stream.go F2 顺序
		// 一致) —— 旧顺序先查外层, 包裹形态的 usage 永远取不到, 计量/统计漏记。
		// P3: 顺带 take-last 暂存, 供 message_start/message_delta 计量回填。
		if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
			lastUsage = u
			if onUsage != nil {
				onUsage(u)
			}
		}

		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			// P3: error 帧即终止 —— 置标志, 收尾的空流守卫与 message_delta/
			// message_stop 据此跳过, 不再补第二条 error。
			upstreamErrored = true
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
			if strings.TrimSpace(rc) != "" {
				reasoningDelivered = true
			}
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

	// P3: 上游 error 帧已终止本流 —— 空流守卫不得再发第二条 error,
	// message_delta/message_stop 收尾也一并跳过(Anthropic 语义里 error 即终止)。
	if upstreamErrored {
		return
	}

	// ★ 空流守卫(2026-09-17 审查 P0-2)。
	//
	// 三条流式入口里, 此前只有 chat 与 responses 有守卫, 本路径**完全没有** ——
	// 上游 200 空手而归时, Claude 协议客户端会拿到一个干净的 end_turn:
	// 不报错、不重试, 任务静默中断。暴露面与 chat 路径一样大, 只是没人给它加过。
	//
	// 交付判据与另两条路径同源(见 stream_delivery.go):
	//   - 有正文 / 有**非空白**推理 / 有**可发出**的工具块 → 已交付
	//   - stop_reason 属于合法空白名单(max_tokens / tool_use) → 合法空回合
	//     (被 token 上限截断、纯工具调用回合, 本就不该有正文, 不能报错)
	//
	// 工具块按"能不能发出"计数: emitToolBlock 会跳过无名的块, 所以
	// pendingTools 非空并不等于有产出。
	emittableTools := 0
	for _, acc := range pendingTools {
		if acc.name != "" {
			emittableTools++
		}
	}
	if !hasText && !reasoningDelivered && emittableTools == 0 &&
		!legitEmptyTerminalReasons[stopReason] {
		log.Printf("  anthropic stream: 上游未交付任何内容(无正文/推理/可发工具), 发 error 而非静默 end_turn")
		emit("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": "empty_content",
				"message": "上游未返回任何内容(整条流无有效 chunk)。这通常是出口节点或上游 worker 异常所致, " +
					"请重试; 若持续出现请更换出口节点。",
			},
		})
		return
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
			// P3: 回填末次上游 usage 的累计输出(协议允许 message_delta 携带累计
			// usage) —— 旧值恒 0, 客户端计费显示全错; 上游从未给 usage 时暂存
			// 为空, 保持 0 与旧实现一致。
			"output_tokens": usageInt("completion_tokens"),
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}
