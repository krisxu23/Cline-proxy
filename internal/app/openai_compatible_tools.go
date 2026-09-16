package app

import "strings"

// 逐字照抄 OmniRoute open-sse/handlers/chatCore/openAICompatibleTools.ts (46 行)。
//
// 原注释 :9-11:
//
//	The Responses translator has dedicated handling for custom, namespace,
//	tool_search, local_shell, and hosted tool types. Normalizing any of them
//	here destroys information before that format-aware conversion can run.
//
// 作用: 把"扁平形态"的工具定义 (顶层 `name` / `parameters` / `input_schema`)
// 归一成 OpenAI chat.completions 要求的 `{type:"function", function:{...}}` 包裹形态,
// 并丢弃既非 function、又没有 function/name 的垃圾条目。
//
// # Go 侧语义对齐要点
//
//   - **OPENAI_RESPONSES 源格式直接原样返回** (`:12-14`): Responses 译码器对
//     custom / namespace / tool_search / local_shell / hosted 等类型有专门处理,
//     在此处归一等于提前毁掉信息。
//   - **filter 与 map 的每个分支必须逐条对应**。filter 保留条件 (`:18-20`):
//     `!tool.type || tool.type === "function" || !!tool.function || !!tool.name`
//     —— 注意 `!!tool.name` 是**真值**判定, 空串 name 不满足。
//   - map 内层 (`:24-30`): 满足 `!tool.type || tool.type === "function" || tool.function`
//     的**原样返回**; 否则才包成 function 形态。
//   - `parameters` 取值优先级 (`:37-39`): `tool.parameters ?? tool.input_schema ?? {}`
//     —— 与 JS `??` 一致, 只在 undefined/null 时回落。**关键**: 两个字段**都不存在**
//     时整个 `parameters` 键不出现 (不是给个 `{}`), 见 `:37` 的 `...? : {}` 展开守卫。
//   - `dropped` 计数 = 过滤前长度 - 归一后长度 (`:45`)。
//
// 常量 FORMats.OPENAI_RESPONSES 在参考实现里是字符串 "openai-responses"
// (见 translator/formats.ts)。Go 侧直接内联字面量, 避免引入未对照的常量表。
const openAIResponsesFormat = "openai-responses"

type openAICompatibleToolResult struct {
	tools   []any
	dropped int
}

// normalizeOpenAICompatibleTools 照抄 openAICompatibleTools.ts:5-46。
func normalizeOpenAICompatibleTools(tools []any, sourceFormat string) openAICompatibleToolResult {
	// :12-14 `if (sourceFormat === FORMATS.OPENAI_RESPONSES) return { tools, dropped: 0 };`
	if sourceFormat == openAIResponsesFormat {
		return openAICompatibleToolResult{tools: tools, dropped: 0}
	}

	before := len(tools)
	normalized := make([]any, 0, len(tools))

	for _, tool := range tools {
		rec, isObj := tool.(map[string]any)
		if !isObj {
			// 参考实现的 filter/map 都是对 `Record<string, unknown>` 做属性访问,
			// 非对象条目属性全为 undefined → `!tool.type` 为真 → **通过 filter**;
			// 随后 map 里 `!tool.type` 仍为真 → **原样返回**。
			// 关键是 JS 对 null 做属性访问会抛错, 但 filter 的 `!tool.type` 在
			// null 上同样会抛 —— 该路径在参考实现里属"调用方保证为对象"。
			// Go 侧选择原样透传而不是 panic, 与 filter 的 `!tool.type`=true 结论一致。
			normalized = append(normalized, tool)
			continue
		}

		// :18-20 filter 保留条件
		if !toolPassesCompatibleFilter(rec) {
			continue // 被丢弃, 不进入 dropped 之外的输出
		}

		// :24-30 map 分支
		if toolNeedsFunctionWrapping(rec) {
			// :32-42 包成 {type:"function", function:{...}}
			fn := map[string]any{"name": rec["name"]}
			// :36 `...(tool.description === undefined ? {} : { description: tool.description })`
			//
			// ★ 关键: 判定是 `=== undefined`, **null 不算 undefined** ——
			// null 会被原样带上 (Node 实测 `{...,"description":null}`)。
			// Go 侧对应: "键是否存在", 而不是"值是否非 nil"。
			if v, ok := rec["description"]; ok {
				fn["description"] = v
			}
			// :37-39 `...(tool.parameters !== undefined || tool.input_schema !== undefined
			//   ? { parameters: tool.parameters ?? tool.input_schema ?? {} } : {})`
			//
			// 两层语义:
			//   - 外层守卫用 `!== undefined`: 任一键存在(哪怕值为 null)就进入,
			//     两个都不存在则整个 parameters 键**不出现**。
			//   - 内层取值用 `??`: 只在 null/undefined 时回落 —— 故
			//     `parameters: null` + `input_schema: {...}` → 取 input_schema。
			params, hasParams := rec["parameters"]
			inputSchema, hasInputSchema := rec["input_schema"]
			if hasParams || hasInputSchema {
				// `tool.parameters ?? tool.input_schema ?? {}`
				if params != nil {
					fn["parameters"] = params
				} else if inputSchema != nil {
					fn["parameters"] = inputSchema
				} else {
					// 两者都是 null/undefined → `?? {}` 兜底空对象
					fn["parameters"] = map[string]any{}
				}
			}
			// :40 `...(tool.strict === undefined ? {} : { strict: tool.strict })`
			//
			// 同 description: `=== undefined` 只排"键不存在", null 保留。
			if v, ok := rec["strict"]; ok {
				fn["strict"] = v
			}
			normalized = append(normalized, map[string]any{
				"type":     "function",
				"function": fn,
			})
			continue
		}

		// :29 `return tool;` —— Responses custom tools 保留原生形态
		normalized = append(normalized, tool)
	}

	// :45 `return { tools: normalized, dropped: before - normalized.length };`
	return openAICompatibleToolResult{tools: normalized, dropped: before - len(normalized)}
}

// toolPassesCompatibleFilter 照抄 openAICompatibleTools.ts:18-20 的 filter 谓词。
//
//	(tool) => !tool.type || tool.type === "function" || !!tool.function || !!tool.name
//
// JS 真值语义对照:
//   - `!tool.type`: type 为 undefined/null/""/0/false 时真
//   - `!!tool.function`: function 为 undefined/null/false/0/"" 时假; 对象/非空串为真
//   - `!!tool.name`: 同上 (空串为假)
func toolPassesCompatibleFilter(rec map[string]any) bool {
	toolType, hasType := rec["type"]
	if !hasType || !isTruthyJS(toolType) {
		return true
	}
	if s, ok := toolType.(string); ok && s == "function" {
		return true
	}
	if v, ok := rec["function"]; ok && isTruthyJS(v) {
		return true
	}
	if v, ok := rec["name"]; ok && isTruthyJS(v) {
		return true
	}
	return false
}

// toolNeedsFunctionWrapping 照抄 openAICompatibleTools.ts:24-30 的分支条件。
//
//	if (!tool.type || tool.type === "function" || tool.function) { return tool; }
//
// 返回 true 表示"需要包成 function 形态"(即走上方的取反分支)。
func toolNeedsFunctionWrapping(rec map[string]any) bool {
	toolType, hasType := rec["type"]
	if !hasType || !isTruthyJS(toolType) {
		return false // !tool.type 为真 → 原样返回
	}
	if s, ok := toolType.(string); ok && s == "function" {
		return false // 原样返回
	}
	if v, ok := rec["function"]; ok && isTruthyJS(v) {
		return false // 原样返回
	}
	return true
}

// isTruthyJS 复刻 JS 的真值语义, 供归一谓词使用。
//
// 参考实现直接用 `!x` / `!!x`, 而 Go 的零值判定与之不同:
//   - JS: "" / 0 / NaN / null / undefined / false 均为 falsy, 其余(含 [] 和 {})为真
//   - Go: 需要显式判空
//
// 只处理工具定义里会出现的类型 (string / bool / 数字 / nil), 数组与对象一律视为真。
func isTruthyJS(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	default:
		// 数组 / 对象 / 其他容器: JS 里恒为真
		return true
	}
}

// sourceFormatIsResponses 判断源格式是否 Responses API 形态, 供调用方选路。
// 属我方新增只读辅助, 不改判定逻辑。
func sourceFormatIsResponses(format string) bool {
	f := strings.ToLower(strings.TrimSpace(format))
	return f == openAIResponsesFormat || f == "responses"
}
