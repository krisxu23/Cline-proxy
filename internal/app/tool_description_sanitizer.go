package app

import "strings"

// 逐字照抄 OmniRoute open-sse/translator/helpers/schemaCoercion.ts:77-81, 226-254, 444-447。
//
// 原注释（逐字, :219-225）:
//
//	/**
//	 * Anthropic's first-party Messages API strictly validates tool `input_schema`
//	 * against JSON Schema draft 2020-12. IDE/SDK agent harnesses that deep-truncate
//	 * their schemas emit invalid constructs — most commonly an array keyword
//	 * (`enum`, `required`, …) replaced by a placeholder string such as
//	 * `"[MaxDepth]"`, or an index-keyed object (`{"0":"a","1":"b"}`) where an array
//	 * is expected. Anthropic rejects these with
//	 * `tools.N.custom.input_schema: JSON schema is invalid` (surfaced as a
//	 * misleading `400 out of extra usage` placeholder when streaming). Non-Anthropic
//	 * targets (OpenAI/Codex) tolerate them, which is why the same request succeeds
//	 * on a fallback provider. This sanitizer coerces or drops the invalid
//	 * constructs so legitimate native-Claude-OAuth traffic is not spuriously
//	 * ...
//
// # 为什么这条对本网关重要
//
// `description` 字段在 Anthropic / MiniMax 这类严格上游上必须是字符串。
// 但 IDE / SDK 类 agent harness 在**深截断**工具定义时会把 description 写成
// `null` 或数字(如 `42`) —— 上游回 400。参考实现把它归一到字符串:
//   - `null`     → `""`（空串, 不是删掉字段）
//   - 数字等其它 → `String(value)`（`42` → `"42"`）
//   - `undefined`(键不存在) → 保持不动（`undefined` 表示"没有这个键"）
//
// ★ 反直觉点（禁止"顺手修正"）: `null` 归一成**空串**而不是**删掉键**。
// 参考实现单测 `schema-coercion.test.ts:76-83` 明确断言 `description === ""`。
// 把 null 当"缺失"删掉会偏离 —— 上游可能把缺字段与空串区别对待。
//
// ★ 覆盖两种形态 + 第三种（Google 的 functionDeclarations）:
//   1. OpenAI 形态: `{type:"function", function:{name, description, parameters}}`
//   2. Anthropic 形态: `{name, description, input_schema}`
//   3. Gemini 形态:   `{functionDeclarations: [{name, description, parameters}]}`
//
// 参考实现的分支条件是 `isPlainObject(result.function) && "description" in
// result.function` —— 注意它**先查 function 分支**, function 不是对象时才看
// 顶层 description。两者互斥, 不会同时对同一份 description 处理两次。

// sanitizeDescriptionValue 照抄 schemaCoercion.ts:77-81。
//
//	function sanitizeDescriptionValue(value: unknown): string | undefined {
//	  if (value === undefined) return undefined;
//	  if (value === null) return "";
//	  return typeof value === "string" ? value : String(value);
//	}
//
// 返回 (值, 是否有值); 第二项为 false 对应参考实现的 `undefined`（"键不存在,
// 不要改动"）。
func sanitizeDescriptionValue(value any) (string, bool) {
	// :78 `if (value === undefined) return undefined;`
	if value == nil {
		// Go 里"键不存在"与"键是 nil"都落到这里。JS 侧 `value === undefined`
		// 只覆盖前者, `undefined` 显式赋值也落在同一分支; JSON 反序列化不会
		// 产生 undefined, 故两者等价。
		return "", false
	}
	// :79 `if (value === null) return "";`
	//
	// ★ JSON 解出来是 nil, 与上面的 Go `nil` 无法区分 —— 见下方 jsNull 哨兵。
	// 这里由调用方用 jsIsExplicitNull 区分后直接给空串。
	// :80 `return typeof value === "string" ? value : String(value);`
	if s, ok := value.(string); ok {
		return s, true
	}
	return jsStringOf(value), true
}

// jsStringOf 复刻 JS 的 `String(value)` 对非字符串、非 null 值的转换。
//
// 只覆盖 JSON 解出来可能出现的类型:
//
//	float64  → `String(42)` = "42"; `String(0.5)` = "0.5"
//	bool     → "true" / "false"
//	[]any    → 数组 join(",")（JS 的 Array.prototype.toString）
//	map      → "[object Object]"
func jsStringOf(v any) string {
	switch t := v.(type) {
	case float64:
		return jsNumberToString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if e == nil {
				// JS: `[null].toString()` = ""（null/undefined 在 join 里变空串）
				parts = append(parts, "")
				continue
			}
			parts = append(parts, jsStringOf(e))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return ""
	}
}

// sanitizeToolDescription 照抄 schemaCoercion.ts:226-254。
func sanitizeToolDescription(tool any) any {
	// :227 `if (!isPlainObject(tool)) return tool;`
	rec, ok := tool.(map[string]any)
	if !ok || !isPlainObject(tool) {
		return tool
	}

	// :229 `const result: JsonRecord = { ...tool };`
	result := shallowCopyStringAny(rec)

	// :231 `if (isPlainObject(result.function) && "description" in result.function) {`
	//
	// ★ 这是**两个独立的 if**, 不是 if/else if。实测差异 (探针 D7): 输入
	//     `{function:{name:"t",description:1}, description:null}`
	//   参考输出 `{"function":{"name":"t","description":"1"},"description":null}`
	//   —— 顶层 `description: null` **保持原样**。第二块的守卫是
	//   `!isPlainObject(result.function)`, 此时 function 是对象故为 false, 跳过。
	//   照抄成 else-if 会在"function 存在但无 description 键"这类输入上与参考
	//   产生分歧, 故严格照抄为两个独立 if。
	fn, fnIsObj := result["function"].(map[string]any)
	fnIsPlain := fnIsObj && isPlainObject(result["function"])
	if fnIsPlain {
		if raw, has := fn["description"]; has {
			// :232 `const description = sanitizeDescriptionValue(result.function.description);`
			desc, hasVal := sanitizeDescriptionValueJS(raw)
			// :233 `if (description !== undefined) {`
			if hasVal {
				// :234 `result.function = { ...result.function, description };`
				nf := shallowCopyStringAny(fn)
				nf["description"] = desc
				result["function"] = nf
			}
		}
	}

	// :238 `if (!isPlainObject(result.function) && "description" in result) {`
	if !fnIsPlain {
		if raw, has := result["description"]; has {
			desc, hasVal := sanitizeDescriptionValueJS(raw)
			if hasVal {
				// :241 `result.description = description;`
				result["description"] = desc
			}
		}
	}

	// :245 `if (Array.isArray(result.functionDeclarations)) {`
	if decls, ok := result["functionDeclarations"].([]any); ok {
		// :246 `result.functionDeclarations = result.functionDeclarations.map(...)`
		next := make([]any, 0, len(decls))
		for _, dRaw := range decls {
			d, isObj := dRaw.(map[string]any)
			if !isObj || !isPlainObject(dRaw) {
				// :247 `if (!isPlainObject(declaration) || !("description" in declaration)) return declaration;`
				next = append(next, dRaw)
				continue
			}
			raw, has := d["description"]
			if !has {
				next = append(next, dRaw)
				continue
			}
			// :248 `const description = sanitizeDescriptionValue(declaration.description);`
			desc, hasVal := sanitizeDescriptionValueJS(raw)
			// :249 `return description === undefined ? declaration : { ...declaration, description };`
			if !hasVal {
				next = append(next, dRaw)
				continue
			}
			nd := shallowCopyStringAny(d)
			nd["description"] = desc
			next = append(next, nd)
		}
		result["functionDeclarations"] = next
	}

	// :253 `return result;`
	return result
}

// sanitizeToolDescriptions 照抄 schemaCoercion.ts:444-447。
//
//	export function sanitizeToolDescriptions(tools: unknown): unknown {
//	  if (!Array.isArray(tools)) return tools;
//	  return tools.map((tool) => sanitizeToolDescription(tool));
//	}
func sanitizeToolDescriptions(tools any) any {
	arr, ok := tools.([]any)
	if !ok {
		return tools
	}
	out := make([]any, 0, len(arr))
	for _, t := range arr {
		out = append(out, sanitizeToolDescription(t))
	}
	return out
}

// sanitizeDescriptionValueJS 在 sanitizeDescriptionValue 之上补上 JS 的
// `null` 与"键不存在"的区分。
//
// JSON 反序列化到 Go 后, `null` 与"键存在但值为 null"都表现为 nil; 而
// `undefined`(键不存在)由调用方用 `_, has := m[k]` 提前排除。因此这里的
// nil 一定是显式 null → 返回空串(参考实现 `:79 if (value === null) return "";`)。
func sanitizeDescriptionValueJS(value any) (string, bool) {
	if value == nil {
		return "", true
	}
	return sanitizeDescriptionValue(value)
}
