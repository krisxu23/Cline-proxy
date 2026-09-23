// Package app — 把「文本形态工具调用」收集成结构化 tool_calls。
//
// 逐字照抄 OmniRoute open-sse/utils/stream.ts 的以下片段:
//   - :261-264  parseTextualToolCallFromContent
//   - :266-268  containsTextualToolCallCandidate
//   - :270-297  containsMalformedTextualToolCall
//   - :299-317  extractAllowedToolNames
//   - :319-339  collectPassthroughTextualToolCall
//   - :2506-2529 在主循环中的三分支消费逻辑(见 collectTextualToolCalls)
//
// 解决的问题: 部分上游把工具调用写进**正文文本**而不返回结构化 tool_calls。
// 网关若不识别, 这段文本会被原样透传给 agent —— agent 既不会执行工具, 也不会
// 报错, 用户看到的就是"任务无缘无故中断"。这里把它转成真正的 tool_calls。
package app

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// textualToolCallRecord 对应参考实现 stream.ts 里的 ToolCall 结构
// (:328-336 构造的那个对象)。
type textualToolCallRecord struct {
	ID        string
	Index     int
	Name      string
	Arguments string
}

// streamRequestTools 取出上游请求体里的 tools 字段。
//
// 参考实现是从 `body` 直接读的(stream.ts:735 `extractAllowedToolNames(body)`)。
// 我方 handleStreamResponseWithUsage 只有一个 *http.Response, 拿不到原始 body,
// 因此改为在出站请求上以内部 header 携带(由 streamToolsHeader 写入)。
// 这是**取数方式的适配**, 判定逻辑完全照抄; 取不到时返回 nil,
// 与参考实现"没有 tools 声明 → 不做白名单过滤"的语义一致。
func streamRequestTools(upstream *http.Response) any {
	if upstream == nil || upstream.Request == nil {
		return nil
	}
	raw := upstream.Request.Header.Get(streamToolsHeaderName)
	if raw == "" {
		return nil
	}
	var tools any
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		return nil
	}
	return tools
}

// streamToolsHeaderName 是网关内部约定: 出站请求上携带客户端声明的工具名清单。
// 用前缀区分业务 header, 避免与上游真实 header 冲突。
const streamToolsHeaderName = "X-Cline-Proxy-Client-Tools"

// snapshotStreamRequestTools 在 cloak/remap 等网关改写**之前**把 body["tools"]
// 序列化成 header 值快照。
//
// 为什么必须"取数即序列化"而不是存 tools 引用、发送时再序列化:
// remapToolNamesInRequest 会**就地**改写共享 tool map 的 name(cloak 虽换新数组,
// 但它排在 remap 之后), 发送时再取到的已是改写后的名字。白名单必须是
// cloak/remap 前的**客户端原始工具名** —— providers_chat.go 在 remap 之前取快照。
//
// 无 tools / 空 / 非数组 / 序列化失败 → 返回 "" —— 读回时为 nil, 等价于参考实现
// 返回 null(不做白名单过滤)。
func snapshotStreamRequestTools(body map[string]any) string {
	tools, ok := body["tools"]
	if !ok || tools == nil {
		return ""
	}
	arr, ok := tools.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return ""
	}
	return string(b)
}

// attachStreamRequestTools 把快照挂到出站请求头。
//
// 拆成"快照/挂载"两段的原因: 出站请求每个 attempt 都要重建(见 providers_chat
// 的 send 闭包), 快照只能在改写前取一次, 挂载则要在每次发送前做。
//
// 为什么用 header 而不是直接传 body: handleStreamResponseWithUsage 只拿到
// *http.Response, 拿不到原始请求体; 而 http.Response.Request 会保留出站请求,
// 于是可以在这里把 tools 顺带捎过去。判定逻辑本身照抄参考实现, 只有取数通道
// 是为适配我方函数签名而选定的。
//
// req 为 nil 或快照为空时什么都不做 —— 读回时为 nil, 等价于参考实现返回 null
// (不做白名单过滤)。
func attachStreamRequestTools(req *http.Request, snapshot string) {
	if req == nil || snapshot == "" {
		return
	}
	req.Header.Set(streamToolsHeaderName, snapshot)
}

// parseTextualToolCallFromContent 照抄 stream.ts:261-264。
//
//	function parseTextualToolCallFromContent(text) {
//	  const candidate = parseTextualToolCallCandidate(text);
//	  return candidate?.kind === "complete" ? { name, args } : null;
//	}
//
// 只有 complete 才返回, partial / null 都当"没拿到调用"。
func parseTextualToolCallFromContent(text any) (string, any, bool) {
	c := parseTextualToolCallCandidate(text)
	if c == nil || c.kind != "complete" {
		return "", nil, false
	}
	return c.name, c.args, true
}

// containsMalformedTextualToolCall 照抄 stream.ts:270-297。
//
//	function containsMalformedTextualToolCall(text, allowedToolNames) {
//	  if (typeof text !== "string") return false;
//	  const normalized = text.replace(/[\u200B-\u200D\uFEFF]/g, "");
//	  let searchIdx = 0;
//	  while (true) {
//	    const idx = normalized.indexOf("[Tool call:", searchIdx);
//	    if (idx === -1) break;
//	    const candidate = normalized.slice(idx);
//	    if (isValidToolCallHeaderPrefix(candidate)) {
//	      const parsed = parseTextualToolCallFromContent(candidate);
//	      if (parsed) {
//	        if (allowedToolNames?.size && !allowedToolNames.has(parsed.name)) return true;
//	      } else {
//	        return true;
//	      }
//	    }
//	    searchIdx = idx + 1;
//	  }
//	  return false;
//	}
//
// 语义: 逐个扫描 "[Tool call:" 出现位置, 只要发现
//   - 头部合法但解析不出完整调用(畸形), 或
//   - 解析出的工具名不在客户端声明的白名单里(幻觉调用)
//
// 就返回 true —— 调用方据此把正文整个清空, 避免把畸形标记喂给 agent。
func containsMalformedTextualToolCall(text any, allowedToolNames map[string]bool) bool {
	s, ok := text.(string)
	if !ok {
		return false
	}
	normalized := zeroWidthRe.ReplaceAllString(s, "")

	searchIdx := 0
	for {
		idx := indexFrom(normalized, toolCallPrefix, searchIdx)
		if idx == -1 {
			break
		}
		candidate := normalized[idx:]
		if isValidToolCallHeaderPrefix(candidate) {
			name, _, parsed := parseTextualToolCallFromContent(candidate)
			if parsed {
				if len(allowedToolNames) > 0 && !allowedToolNames[name] {
					return true
				}
			} else {
				return true
			}
		}
		searchIdx = idx + 1
	}
	return false
}

// indexFrom 是 JS `normalized.indexOf(needle, from)` 的等价物:
// 从 from 开始找 needle, 找不到返回 -1。
func indexFrom(haystack, needle string, from int) int {
	if from < 0 {
		from = 0
	}
	if from > len(haystack) {
		return -1
	}
	sub := strings.Index(haystack[from:], needle)
	if sub == -1 {
		return -1
	}
	return from + sub
}

// extractAllowedToolNames 照抄 stream.ts:299-317。
//
//	function extractAllowedToolNames(body) {
//	  const tools = asRecord(body).tools;
//	  if (!Array.isArray(tools)) return null;
//	  const names = new Set();
//	  for (const tool of tools) {
//	    if (!tool || typeof tool !== "object" || Array.isArray(tool)) continue;
//	    const directName = typeof tool.name === "string" ? tool.name.trim() : "";
//	    const fn = (tool.function && typeof tool.function === "object" && !Array.isArray(tool.function))
//	      ? tool.function : null;
//	    const functionName = typeof fn?.name === "string" ? fn.name.trim() : "";
//	    const name = functionName || directName;   // function.name 优先
//	    if (name) names.add(name);
//	  }
//	  return names.size > 0 ? names : null;
//	}
//
// 要点: function.name 优先于顶层 name (兼容 chat.completions 的 function 包裹
// 与 Responses 的无包裹两种形态); 结果为空时参考实现返回 null(不过滤)。
// 这里用"空 map + 调用方判 len()"表达同一语义。
func extractAllowedToolNames(tools any) map[string]bool {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for _, t := range arr {
		item, ok := t.(map[string]any)
		if !ok {
			continue
		}
		directName := ""
		if s, ok := item["name"].(string); ok {
			directName = trimSpace(s)
		}
		functionName := ""
		if fn, ok := item["function"].(map[string]any); ok {
			if s, ok := fn["name"].(string); ok {
				functionName = trimSpace(s)
			}
		}
		name := functionName
		if name == "" {
			name = directName
		}
		if name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// trimSpace 对应 JS 的 String.prototype.trim()。
// 工具名只可能由 ASCII 空白分隔, 与 strings.TrimSpace 行为一致。
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// collectTextualToolCalls 对应 stream.ts:2506-2529 的消费逻辑。
//
//	:2509-2520 先冲掉缓冲区里攒着的文本形态调用
//	:2521-2526 content 能收集成调用 → 从正文摘除, 置 hasToolCalls
//	:2527-2528 收集失败但形态畸形 → 清空正文, 避免喂给 agent
//
// 返回值 hasToolCalls 表示本次是否**新收集到**工具调用(调用方据此把该帧
// 计入 forwardedValuableChunk —— 工具调用本身就是交付价值)。
//
// 与参考实现的差异(取数适配): 参考实现里 tool_calls 最终写回 message,
// 这里写回 chunk 的 choices[0].delta.tool_calls, 因为我方处于**流式转发**
// 路径, 客户端消费的是 delta。补齐方式见 flushTextualToolCallsToDelta。
func collectTextualToolCalls(chunk map[string]any, toolCalls map[string]textualToolCallRecord, allowedToolNames map[string]bool) bool {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	// 优先看 delta(流式), 退回 message(非流式形态)
	var holder map[string]any
	for _, key := range []string{"delta", "message"} {
		if h, ok := first[key].(map[string]any); ok {
			holder = h
			break
		}
	}
	if holder == nil {
		return false
	}

	rawContent, hasContent := holder["content"]
	if !hasContent {
		return false
	}
	content, isStr := rawContent.(string)
	if !isStr || content == "" {
		return false
	}

	// 分支结构严格对齐参考实现 stream.ts:2521-2529:
	//
	//	if (content && collectPassthroughTextualToolCall(content, calls, allowed)) {
	//	  hasToolCalls = true;
	//	  content = "";
	//	} else if (containsMalformedTextualToolCall(content, allowed)) {
	//	  content = "";
	//	}
	//
	// ⚠ 关键: collectPassthroughTextualToolCall 在**名字不在白名单**时返回 null
	// (:326 `if (allowedToolNames?.size && !allowedToolNames.has(parsed.name)) return null;`)。
	// 此时必须**落到 else if 分支**, 由 containsMalformedTextualToolCall 判为畸形
	// (:286-288 对同一条件返回 true) 并清空正文。
	//
	// 我第一版把白名单判断嵌在 parse 成功分支内部, 导致"解析得出但不在白名单"
	// 既不收集也不清空 —— 畸形标记会原样漏给 agent。Node 实跑确认参考实现终态
	// 是 content = ""。
	collected := false
	if name, args, ok := parseTextualToolCallFromContent(content); ok {
		if len(allowedToolNames) == 0 || allowedToolNames[name] {
			appendTextualToolCall(toolCalls, name, args)
			// :2526 —— 摘除正文, 避免同一段文本既当内容又当调用
			holder["content"] = nil
			collected = true
		} else if containsMalformedTextualToolCall(content, allowedToolNames) {
			// 名字不在白名单: collect 返回 null → 落 else if → 清空
			holder["content"] = nil
		}
	} else if containsMalformedTextualToolCall(content, allowedToolNames) {
		// :2528 —— 畸形标记必须清空, 否则 agent 会把它当普通文本读取
		holder["content"] = nil
	}

	if len(toolCalls) == 0 {
		return collected
	}
	// 把已收集的调用写回本帧 delta, 客户端才能看到 tool_calls
	holder["tool_calls"] = textualToolCallsToDelta(toolCalls)
	return true
}

// appendTextualToolCall 对应 stream.ts:327-337:
//
//	const key = `textual:${toolCalls.size}`;
//	const toolCall = {
//	  id: `call_${Date.now()}_${toolCalls.size}`,
//	  index: toolCalls.size,
//	  type: "function",
//	  function: { name: parsed.name, arguments: JSON.stringify(parsed.args || {}) },
//	};
//	toolCalls.set(key, toolCall);
//
// arguments 用 JSON.stringify(args || {}) —— args 为 nil 时退化成 "{}"。
func appendTextualToolCall(toolCalls map[string]textualToolCallRecord, name string, args any) {
	idx := len(toolCalls)
	argsJSON := "{}"
	if args != nil {
		if b, err := json.Marshal(args); err == nil {
			argsJSON = string(b)
		}
	}
	key := "textual:" + strconv.Itoa(idx)
	toolCalls[key] = textualToolCallRecord{
		ID:        "call_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + strconv.Itoa(idx),
		Index:     idx,
		Name:      name,
		Arguments: argsJSON,
	}
}

// textualToolCallsToDelta 对应 stream.ts:341-351 toStreamingToolCallDelta,
// 并按参考实现 :2538-2541 的 `[...toolCalls.values()].sort((a,b)=>a.index-b.index)`
// 保证顺序稳定。
func textualToolCallsToDelta(toolCalls map[string]textualToolCallRecord) []any {
	records := make([]textualToolCallRecord, 0, len(toolCalls))
	for _, r := range toolCalls {
		records = append(records, r)
	}
	// 按 index 升序(插入序即 index 序): 每帧都要重建, 用 sort.Slice (O(k log k))
	// 而非 O(k^2) 选择排序; 同样不依赖 map 遍历顺序。
	sort.Slice(records, func(i, j int) bool {
		return records[i].Index < records[j].Index
	})
	out := make([]any, 0, len(records))
	for _, r := range records {
		out = append(out, map[string]any{
			"index": r.Index,
			"id":    r.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      r.Name,
				"arguments": r.Arguments,
			},
		})
	}
	return out
}
