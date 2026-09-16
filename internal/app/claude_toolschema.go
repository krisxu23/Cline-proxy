package app

import "strings"

// 逐字照抄 OmniRoute:
//
//	open-sse/translator/request/claude-to-openai.ts
//
// 本文件收两小段：**Claude→OpenAI 请求转换咽喉**里的两处归一。
// 我方对位位置是 `anthropic.go:anthropicToolsToOpenAI`（工具）与
// `anthropic.go:anthropicToOpenAI`（顶层字段）。
//
// 为什么必须照抄这两段（本文件的核心业务价值）：
//
// 一、`normalizeToolSchema`
//
// 参考实现原注释（逐字）：
//
//	/**
//	 * Normalize tool input schema for OpenAI compatibility.
//	 * OpenAI strict mode requires `properties: {}` on object-type schemas,
//	 * even for zero-argument tools. Anthropic/MCP tools may omit it (#1898).
//	 */
//
// Anthropic 允许工具 schema 写成 `{"type":"object"}` 而**省略** `properties`
// （零参数工具尤其常见），MCP 工具也会这么发。OpenAI 的 strict 校验不接受 ——
// 上游回 400 `tools.N.function.parameters` 相关错误。在 agent 客户端里表现为
// 任务无声中断，与用户报的"无缘无故中断"同源。
//
// 二、`normalizeOpenAIReasoningEffort` + reasoning_effort 分档
//
// 参考实现原注释（逐字）：
//
//	// Reasoning effort: map Claude-side thinking controls to OpenAI reasoning_effort.
//	// Priority: output_config.effort (Claude Code) > thinking.budget_tokens (Claude native).
//	// Budget buckets match the reverse mapping in thinkingBudget.ts::setCustomBudget.
//
// Claude Code 客户端用 `output_config.effort` 表达推理强度，Claude 原生客户端用
// `thinking.budget_tokens`。二者都是 Anthropic 方言，OpenAI 方言对应
// `reasoning_effort`。不翻译则上游收到一个它不认识的字段（被静默忽略或直接 400），
// 模型实际跑在默认强度上 —— 用户以为自己调过档，实际没有。
//
// ★ 分档阈值已与 `services/thinkingBudget.ts:316-324` 的**反向映射**逐值核对一致：
// low ≤ 1024 / medium ≤ 10240 / high < 131072 / xhigh 其余。
//
// ★ 照抄时**有意不抄**的部分（禁止为了"看起来完整"而留死代码）：
// `convertClaudeServerWebSearchTool` / `isClaudeServerWebSearchTool` /
// `hasClaudeServerWebSearchTool`（claude-to-openai.ts:42-83, 调用点 :217-219）
// 在参考实现里被 `shouldUseNativeResponsesWebSearch(credentials)` 门控，条件是
// `credentials._targetFormat === FORMATS.OPENAI_RESPONSES`，即**上游**走 OpenAI
// Responses API。本网关的上行端点恒为 `/chat/completions`
// （见 providers_config.go:152 `return base + "/chat/completions"`），
// `shapeResponses` 只用于**下行**（回复客户端的形状，见 responses.go:461/474）。
// 因此该分支在本网关恒为 false —— 抄过来就是永远不触发的死代码，按本仓库纪律不接。

// normalizeToolSchema 照抄 claude-to-openai.ts:26-34。
//
//	const fallback = { type: "object", properties: {} };
//	if (!schema || typeof schema !== "object" || Array.isArray(schema)) return fallback;
//	const s = schema as Record<string, unknown>;
//	if (s.type === "object" && !s.properties) {
//	    return { ...s, properties: {} };
//	}
//	return s;
//
// Go/JS 差异说明：
//   - `!schema` 覆盖 JS falsy（nil / "" / 0 / false）→ 用 jsTruthy 判。
//   - `typeof schema !== "object" || Array.isArray(schema)` → 用 map 断言判，
//     `[]any` / string / float64 / bool 全部落到 fallback。
//   - `!s.properties` 同样覆盖 JS falsy：键缺失、nil、false、0、"" 都要补。
//   - `{ ...s, properties: {} }` 是**浅拷贝** —— 必须返回新 map，不得原地改，
//     否则客户端复用同一份请求（回退重试）时会被我们改出多余字段。
func normalizeToolSchema(schema any) any {
	emptyObjectSchema := map[string]any{"type": "object", "properties": map[string]any{}}
	if !jsTruthy(schema) {
		return emptyObjectSchema
	}
	s, ok := schema.(map[string]any)
	if !ok {
		return emptyObjectSchema
	}
	if s["type"] == "object" && !jsTruthy(s["properties"]) {
		out := make(map[string]any, len(s)+1)
		for k, v := range s {
			out[k] = v
		}
		out["properties"] = map[string]any{}
		return out
	}
	return s
}

// normalizeOpenAIReasoningEffort 照抄 claude-to-openai.ts:36-40。
//
//	if (typeof effort !== "string") return undefined;
//	const normalized = effort.toLowerCase();
//	return normalized || undefined;
//
// Go 侧用空串表达 `undefined`（调用方以 `!= ""` 判存在）。
//
// ★ 反直觉项（探针 P2h，禁止"顺手修正"）：`"  "`（纯空格）
// lower 之后仍是 `"  "`，非空 → **原样返回**。只有空串才归 undefined。
func normalizeOpenAIReasoningEffort(effort any) string {
	s, ok := effort.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(s)
}

// openAIReasoningEffort 照抄 claude-to-openai.ts:252-271 的分档逻辑。
//
// 参考实现原文（逐字，仅把就地写 result 改成返回字符串）：
//
//	const outputEffort = normalizeOpenAIReasoningEffort(body.output_config?.effort) || "";
//	if (outputEffort) {
//	    result.reasoning_effort = outputEffort;
//	} else if (body.thinking?.type === "enabled" && typeof body.thinking.budget_tokens === "number") {
//	    const budget = body.thinking.budget_tokens;
//	    if (budget <= 0) {
//	        // disabled — leave reasoning_effort unset
//	    } else if (budget <= 1024) {
//	        result.reasoning_effort = "low";
//	    } else if (budget <= 10240) {
//	        result.reasoning_effort = "medium";
//	    } else if (budget < 131072) {
//	        result.reasoning_effort = "high";
//	    } else {
//	        result.reasoning_effort = "xhigh";
//	    }
//	}
//
// 返回 "" 表示"不要设置 reasoning_effort"（探针 P3b/P3c/P3k/P3l/P3m/P3n/P3o）。
//
// ★ 优先级是照抄的：output_config.effort（Claude Code 方言）**严格优先**于
//
//	thinking.budget_tokens（Claude 原生方言）。两者同时存在时只看前者。
//
// ★ P3p 的反直觉行为（禁止"顺手修正"）：`output_config.effort = ""`
//
//	经 `normalizeOpenAIReasoningEffort` 归成 undefined、再 `|| ""` 得 ""，
//	于是 **fall through 到 thinking 分支**。不是"output_config 存在就屏蔽 thinking"，
//	而是"output_config 有**有效** effort 才屏蔽"。
func openAIReasoningEffort(body map[string]any) string {
	outputConfig, _ := body["output_config"].(map[string]any)
	if effort := normalizeOpenAIReasoningEffort(outputConfig["effort"]); effort != "" {
		return effort
	}

	thinking, _ := body["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" {
		return ""
	}
	budget, ok := thinking["budget_tokens"].(float64)
	if !ok {
		return ""
	}
	switch {
	case budget <= 0:
		// disabled — leave reasoning_effort unset
		return ""
	case budget <= 1024:
		return "low"
	case budget <= 10240:
		return "medium"
	case budget < 131072:
		return "high"
	default:
		return "xhigh"
	}
}

// convertToolChoice 照抄 claude-to-openai.ts:537-554。
//
// 参考实现原文（逐字）：
//
//	const TOOL_CHOICE_ANY = ["a", "n", "y"].join("");
//	function convertToolChoice(choice, hasServerWebSearch = false) {
//	  if (!choice) return "auto";
//	  if (typeof choice === "string") return choice;
//	  switch (choice.type) {
//	    case "auto":      return "auto";
//	    case TOOL_CHOICE_ANY:  return "required";
//	    case "tool":
//	      if (hasServerWebSearch && choice.name === "web_search")
//	        return { type: "web_search" };
//	      return { type: "function", function: { name: choice.name } };
//	    default: return "auto";
//	  }
//	}
//
// ★ 有意去掉第二参数 hasServerWebSearch（不留死代码纪律）：
//
//	参考实现里该参数的真值来自
//	`useNativeResponsesWebSearch && hasClaudeServerWebSearchTool(body.tools)`，
//	而 `useNativeResponsesWebSearch` 要求上游走 OpenAI Responses API
//	（见本文件头部对 convertClaudeServerWebSearchTool 的说明）。本网关上行端点
//	恒为 /chat/completions → 恒 false → web_search 分支不可达。去掉参数后，
//	后续若真的接入 Responses 上游，再按需补回该分支即可。
//
// ★ 我方入参是 json.RawMessage（req.ToolChoice），调用方先 Unmarshal 成 any 再喂进来。
//
//	JS 的 `typeof choice === "string"` 对位 Go 的 `choice.(string)`；
//	`choice.type` 对位 `m["type"]`（choice 为 map[string]any 时）。
//
// ★ 返回值保持 any：字符串形态（"auto"/"required"）与对象形态
//
//	（{type:"function", function:{name:...}}）共用同一返回类型。
func convertToolChoice(choice any) any {
	if !jsTruthy(choice) {
		return "auto"
	}
	if s, ok := choice.(string); ok {
		return s
	}
	m, ok := choice.(map[string]any)
	if !ok {
		return "auto"
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any": // TOOL_CHOICE_ANY
		return "required"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": m["name"]},
		}
	default:
		return "auto"
	}
}
