package app

import (
	"encoding/json"
	"net/http"
	"strings"
)

// opencode zen 免费层的形态要求(2026-09-22 直连实测, Bearer public + Bearer <key> 双通过):
//
//	free 模型(带 -free 后缀 / seed 白名单 / 手动启用)经 zen 上游时, 请求体必须
//	同时满足:
//	  1. "stream": true                     —— 非流式一律 403 FreeTierError
//	  2. 至少一个 agent tool 定义            —— 空 tools / 非 agent 工具一律 403
//	  3. 官方 CLI 身份头(x-opencode-session 等) —— 由 opencode_headers.go 处理
//
//	opencode2api (jasonxu114514) 用同样规则修复:
//	匿名通道强制注入 bash/edit/glob/grep/read 五件套, 并强制 stream。
//	本文件对齐同一口径。
//
//	★ FreeTierError 与身份头 / 出口 IP 无关(至少在当前规则下)。
//	本仓库 docs/opencode-zen-facts.md 第 4 节"A/B/C 三向实测"的结论是**过期**的:
//	当时的实验 body 都缺 tools 与 stream, 因此身份头怎么改都拿不到 200。
//	补齐 body 形态后, 同一 IP 同一 key 同一 UA 即通过。
//
//	★ 免费层判定只看模型 ID: 免费付费共用同一批 key, 但请求体形态只有免费层
//	需要, 给付费模型强行注入 agent tools 反而可能触发上游的"agent 语义"分支
//	(不预期 tool_call 回包)。
//
//	★ /chat/completions、/responses、/messages 三个端点都要求 agent 形态。
//	上游按端点翻译 body 形状, 但门禁规则一致: 缺 tools 或 stream 即 403。
//
//	三个端点的 tool 形状不同 —— 用错形状会被上游 400:
//	  chat:     {"type":"function","function":{"name":...,"parameters":...}}  (嵌套)
//	  responses:{"type":"function","name":...,"parameters":...}                (扁平)
//	  messages: {"name":...,"description":...,"input_schema":...}              (claude)

// zenAgentCoreTools 免费层要求的 agent 工具集, 顺序与 opencode2api 一致。
var zenAgentCoreTools = []string{"bash", "edit", "glob", "grep", "read"}

// zenAgentToolChatDefinition 生成 chat 协议的 OpenAI function tool 定义。
// /chat/completions 严格要求 function 嵌套; 扁平形状会被上游拒 400
// "tools[0].function must be an object"。
func zenAgentToolChatDefinition(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Agent tool " + name,
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// zenAgentToolResponsesDefinition 生成 /responses 端点的 tool 定义(扁平)。
// /responses 严格要求 tool 是 {"type","name","parameters"} 的扁平形状;
// chat 的 function 嵌套形状会被上游拒 400 "tools[0] missing required field name"。
func zenAgentToolResponsesDefinition(name string) map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": "Agent tool " + name,
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// zenAgentToolMessagesDefinition 生成 /messages 端点的 tool 定义(claude 形状)。
// OpenAIChatToClaudeRequest 会把 chat 的 tools 翻译为 {"name","description",
// "input_schema"}, 我们注入时保持同一形状, 避免重复翻译。
func zenAgentToolMessagesDefinition(name string) map[string]any {
	return map[string]any{
		"name":         name,
		"description":  "Agent tool " + name,
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// zenBodyToolName 提取工具定义里的 name 字段。兼容 chat(嵌套 function)、
// responses(扁平)与 messages(input_schema)三种形状。
func zenBodyToolName(t any) string {
	m, ok := t.(map[string]any)
	if !ok {
		return ""
	}
	if n, ok := m["name"].(string); ok && n != "" {
		return n
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && n != "" {
			return n
		}
	}
	return ""
}

// zenBodyToolNames 从 tools 字段提取全部工具名(去重、保序)。
func zenBodyToolNames(body map[string]any) map[string]bool {
	raw, ok := body["tools"].([]any)
	if !ok {
		return nil
	}
	present := make(map[string]bool, len(raw))
	for _, t := range raw {
		if n := zenBodyToolName(t); n != "" {
			present[n] = true
		}
	}
	return present
}

// zenBodyIsStream 读出 body 里当前的 stream 值(缺省视为 false)。
func zenBodyIsStream(body map[string]any) bool {
	if v, ok := body["stream"].(bool); ok {
		return v
	}
	return false
}

// zenFreeShapeRequired 报告请求是否必须被整形为 agent 形态。
// 仅免费层路径需要 —— 付费模型保持客户端原样语义, 避免误触发上游 tool_call
// 分支。返回 false 时 zenApplyFreeShape 是纯透传, 无副作用。
func zenFreeShapeRequired(endpoint zenEndpointKind, modelID string) bool {
	// 三个已知端点 (chat/responses/messages) 都可能命中免费层形态要求。
	// 其它端点 (gemini 等) 目前不接入免费层校验, 保持透传。
	if endpoint == zenEndpointGemini {
		return false
	}
	if strings.HasSuffix(modelID, "-free") {
		return true
	}
	m, ok := resolveZenModel(modelID)
	if !ok {
		return false
	}
	return isZenFreeModel(m)
}

// zenAgentDefinitionsByEndpoint 返回该端点上要注入的 tool 定义生成器。
// 三个端点形状不同 —— 见文件顶部注释。
func zenAgentDefinitionsByEndpoint(endpoint zenEndpointKind) func(string) map[string]any {
	switch endpoint {
	case zenEndpointResponses:
		return zenAgentToolResponsesDefinition
	case zenEndpointMessages:
		return zenAgentToolMessagesDefinition
	default:
		return zenAgentToolChatDefinition
	}
}

// zenApplyFreeShape 把 zen 免费模型请求整形为 agent 形态:
//
//  1. 强制 stream=true。上游永远按 SSE 返回; 若客户端原始请求非流式, 调用方
//     在拿到响应后按 forcedStream 标志把 SSE 汇总为 JSON。
//  2. 追加缺失的 agent 工具(bash/edit/glob/grep/read); 已存在的同名工具不重复。
//     客户端自己的工具定义保持原样。
//  3. chat 端点: stream_options.include_usage=true, 上游在最终 chunk 带 usage。
//     responses/messages 端点不需要该字段。
//
// endpoint 是 zenEndpointChat / zenEndpointResponses / zenEndpointMessages 之一,
// 决定 tools 的形状与是否需要 stream_options。
//
// 返回新的 body 与是否强制了 stream(需要 SSE→JSON 汇总)。
// 非免费层或端点不匹配时 (body, false) 原样返回。
func zenApplyFreeShape(endpoint zenEndpointKind, body map[string]any) (map[string]any, bool) {
	modelID, _ := body["model"].(string)
	if !zenFreeShapeRequired(endpoint, modelID) {
		return body, false
	}

	wasStream := zenBodyIsStream(body)
	if !wasStream {
		body["stream"] = true
	}

	// 追加缺失的 agent 工具。已存在的保留原定义, 只补缺。
	newToolDef := zenAgentDefinitionsByEndpoint(endpoint)
	present := zenBodyToolNames(body)
	var missing []string
	if present == nil {
		missing = append(missing, zenAgentCoreTools...)
	} else {
		for _, n := range zenAgentCoreTools {
			if !present[n] {
				missing = append(missing, n)
			}
		}
	}
	if len(missing) > 0 {
		var tools []any
		if raw, ok := body["tools"].([]any); ok {
			tools = raw
		}
		added := make([]any, 0, len(missing))
		for _, n := range missing {
			added = append(added, newToolDef(n))
		}
		body["tools"] = append(tools, added...)
	}

	// 仅 chat 端点需要 stream_options.include_usage; responses/messages 端点
	// 上游不需要这个字段, 加了反而会与 validateOutbound 冲突。
	if endpoint == zenEndpointChat {
		if opts, ok := body["stream_options"].(map[string]any); ok {
			if inc, ok := opts["include_usage"].(bool); !ok || !inc {
				opts["include_usage"] = true
			}
		} else {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	return body, !wasStream
}

// zenCollapseFreeStreamToJSON 把被免费层强制成 stream 的上游 chat SSE 响应
// 汇总为 JSON body。仅当客户端原始请求是非流式时被调用 —— 否则透传给 handler
// 的 handleStreamResponseWithUsage。
//
// 实现复用 collectStreamResponse(既有 SSE 汇总器), 再把结果合成 JSON
// 形态的 *http.Response。这样所有 callZenAPI 的调用方无需感知免费层的
// 强制 stream 语义, 与非免费层的调用形态完全一致。
func zenCollapseFreeStreamToJSON(upstream *http.Response) (*http.Response, error) {
	defer upstream.Body.Close()
	collapsed, err := collectStreamResponse(upstream)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(collapsed)
	if err != nil {
		return nil, err
	}
	return synthesizeChatJSONResponse(out), nil
}
