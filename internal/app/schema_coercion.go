package app

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/schemaCoercion.ts (640 行)
// 的两个导出函数及其依赖:
//
//	stripUnsupportedRegexPatterns  (schemaCoercion.ts:187-223)
//	stripInvalidSchemaConstructs   (schemaCoercion.ts:534-623)
//	sanitizeClaudeToolSchema       (:625-632)
//	sanitizeClaudeToolSchemas      (:634-639)
//
// 依赖的常量与私有辅助均逐条照抄(见各定义处行号)。
//
// # 为什么这两个函数值得抄(用户场景)
//
// 1. `stripUnsupportedRegexPatterns` —— 原注释 :178-182:
//    "Strip regex `pattern` constraints that use lookaround (lookahead/lookbehind),
//     which OpenAI/Codex's Responses API rejects outright with a 400
//     ("Invalid JSON schema: regex lookaround is not supported.")"
//    IDE/SDK 的 agent 工具链**经常**发出带 lookahead 的 pattern
//    (例如 `^(?=.*@).+$` 校验邮箱)。用户正是通过 Codex 使用网关, 这条会真实触发:
//    工具 schema 里只要有一个 lookaround 就整个请求 400, 表现为工具完全不可用。
//
// 2. `stripInvalidSchemaConstructs` —— 把 IDE/SDK 送来的畸形 JSON Schema 构造
//    归一到严格上游能接受的形态: 字符串数字约束转数字(Anthropic 拒绝字符串形式)、
//    非数组的数组关键字丢弃(`enum: "[MaxDepth]"`)、占位符字符串 `[MaxDepth]` 换成
//    `{}`、布尔子 schema 保留(不能强行换成 `{}`, 否则静默放开额外字段并诱发模型
//    幻觉参数)。
//
// # Go 侧语义对齐要点
//
//   - 两个函数都**返回新对象, 不修改入参**(用 `{...schema}` 浅拷贝起步)。
//   - 非对象非数组输入: `stripInvalidSchemaConstructs` 对占位符字符串返回 `{}`,
//     其余原样返回(:534-538); `stripUnsupportedRegexPatterns` 一律原样返回(:190)。
//   - 数值转换只认**纯数字字符串**(trim 后 `Number()` 有限), 其余保留原值。
//   - `sanitizeClaudeToolSchema` 注释 :627-631 明确说**故意不组合**
//     `coerceSchemaNumericFields`(那个会剥掉合法的 `default` 关键字, 属译码器职责),
//     我方照抄这一决定。

// numericSchemaFields 照抄 schemaCoercion.ts:14-26 的 NUMERIC_SCHEMA_FIELDS。
var numericSchemaFields = []string{
	"minimum",
	"maximum",
	"exclusiveMinimum",
	"exclusiveMaximum",
	"minLength",
	"maxLength",
	"minItems",
	"maxItems",
	"minProperties",
	"maxProperties",
	"multipleOf",
}

// regexLookaroundRe 照抄 schemaCoercion.ts:30 的 REGEX_LOOKAROUND_PATTERN
// `/\(\?<?[=!]/` —— 匹配 `(?=`, `(?!`, `(?<=`, `(?<!`。
var regexLookaroundRe = regexp.MustCompile(`\(\?<?[=!]`)

// regexStripObjectMapFields 照抄 schemaCoercion.ts:163-168。
var regexStripObjectMapFields = []string{
	"properties",
	"patternProperties",
	"definitions",
	"$defs",
}

// regexStripArrayMapFields 照抄 schemaCoercion.ts:171。
var regexStripArrayMapFields = []string{
	"prefixItems",
	"anyOf",
	"oneOf",
	"allOf",
}

// schemaPlaceholderRe 照抄 schemaCoercion.ts:503
// `/^\[(?:MaxDepth|Truncated|Circular|Object|Array)\]$/`。
var schemaPlaceholderRe = regexp.MustCompile(`^\[(?:MaxDepth|Truncated|Circular|Object|Array)\]$`)

// arraySchemaKeys 照抄 schemaCoercion.ts:504。
var arraySchemaKeys = []string{"enum", "required", "anyOf", "oneOf", "allOf", "prefixItems"}

// schemaArrayOfSchemas 照抄 schemaCoercion.ts:505(Set)。
var schemaArrayOfSchemas = map[string]bool{
	"anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true,
}

// schemaSlotKeys 照抄 schemaCoercion.ts:506-517。
var schemaSlotKeys = []string{
	"items",
	"additionalProperties",
	"propertyNames",
	"contains",
	"not",
	"if",
	"then",
	"else",
	"unevaluatedProperties",
	"additionalItems",
}

// --- 私有辅助(逐条照抄) ---

// 注: isPlainObject 已由 tool_schema_sanitizer.go:29 定义(同一语义, 同为
// `Boolean(value) && typeof value === "object" && !Array.isArray(value)` 的照抄),
// 此处不重复定义。

// hasUnsupportedRegexLookaround 照抄 :35-37。
func hasUnsupportedRegexLookaround(pattern any) bool {
	s, ok := pattern.(string)
	if !ok {
		return false
	}
	return regexLookaroundRe.MatchString(s)
}

// coerceNumericString 照抄 :62-69。
//
//	function coerceNumericString(value) {
//	  if (typeof value !== "string") return value;
//	  const trimmed = value.trim();
//	  if (trimmed.length === 0) return value;
//	  const parsed = Number(trimmed);
//	  return Number.isFinite(parsed) ? parsed : value;
//	}
//
// ★ 注意: 非字符串**原样返回**(不做反向转换); 空串(trim 后)原样返回;
// 能转成有限数的字符串才转。`Number("0x10")` 在 JS 里是 16, 但 JSON Schema
// 不会出现这种形态, 故 Go 侧用 strconv.ParseFloat(不接受 0x 前缀)。
func coerceNumericString(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return value
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return value
	}
	return f
}

// --- stripUnsupportedRegexPatterns ---

// stripUnsupportedRegexPatterns 照抄 schemaCoercion.ts:187-223。
func stripUnsupportedRegexPatterns(schema any) any {
	// :188-190 `if (Array.isArray(schema)) return schema.map(...);`
	if arr, ok := schema.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, entry := range arr {
			out = append(out, stripUnsupportedRegexPatterns(entry))
		}
		return out
	}
	// :191 `if (!isPlainObject(schema)) return schema;`
	rec, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	// :193 `const result: JsonRecord = { ...schema };`
	result := shallowCopyRecord(rec)

	// :195-197 `if (hasUnsupportedRegexLookaround(result.pattern)) delete result.pattern;`
	if hasUnsupportedRegexLookaround(result["pattern"]) {
		delete(result, "pattern")
	}

	// :199-203 object-map 字段
	for _, field := range regexStripObjectMapFields {
		if sub, ok := result[field].(map[string]any); ok {
			mapped := make(map[string]any, len(sub))
			for k, v := range sub {
				mapped[k] = stripUnsupportedRegexPatterns(v)
			}
			result[field] = mapped
		}
	}

	// :205-211 array-map 字段
	for _, field := range regexStripArrayMapFields {
		if arr, ok := result[field].([]any); ok {
			out := make([]any, 0, len(arr))
			for _, entry := range arr {
				out = append(out, stripUnsupportedRegexPatterns(entry))
			}
			result[field] = out
		}
	}

	// :213-215 items
	if v, ok := result["items"]; ok && v != nil {
		result["items"] = stripUnsupportedRegexPatterns(v)
	}
	// :216-218 additionalProperties (仅对象形态; 布尔保留)
	if v, ok := result["additionalProperties"]; ok && isPlainObject(v) {
		result["additionalProperties"] = stripUnsupportedRegexPatterns(v)
	}
	// :219-221 not
	if v, ok := result["not"]; ok && isPlainObject(v) {
		result["not"] = stripUnsupportedRegexPatterns(v)
	}

	return result
}

// --- stripInvalidSchemaConstructs ---

// coerceIndexedObjectToArray 照抄 :519-528。
//
//	把"键恰好是 0,1,2,... 的对象"视作数组(IDE 序列化丢类型的常见形态);
//	键顺序不影响判定(用 String(index) === key 逐个比对)。
func coerceIndexedObjectToArray(value any) []any {
	if arr, ok := value.([]any); ok {
		return arr
	}
	rec, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	// `keys.every((key, index) => String(index) === key)` —— 与遍历顺序无关,
	// 逐个检查 key 是否是某个有效下标。
	for _, k := range keys {
		idx, err := strconv.Atoi(k)
		if err != nil || idx < 0 || idx >= len(keys) {
			return nil
		}
	}
	out := make([]any, len(keys))
	for _, k := range keys {
		idx, _ := strconv.Atoi(k)
		out[idx] = rec[k]
	}
	return out
}

// isSchemaPlaceholder 照抄 :530-532。
func isSchemaPlaceholder(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	return schemaPlaceholderRe.MatchString(strings.TrimSpace(s))
}

// stripInvalidSchemaConstructs 照抄 schemaCoercion.ts:534-623。
func stripInvalidSchemaConstructs(schema any) any {
	// :535-537 `if (Array.isArray(schema)) return schema.map(...);`
	if arr, ok := schema.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, entry := range arr {
			out = append(out, stripInvalidSchemaConstructs(entry))
		}
		return out
	}
	// :538-540 `if (!isPlainObject(schema)) return isSchemaPlaceholder(schema) ? {} : schema;`
	rec, isRec := schema.(map[string]any)
	if !isRec {
		if isSchemaPlaceholder(schema) {
			return map[string]any{}
		}
		return schema
	}

	// :542 `const result: JsonRecord = {};` —— 注意是**全新空对象**, 逐键重建
	result := make(map[string]any, len(rec))
	for key, value := range rec {
		// :547-550 数值约束键: 字符串数字转数值
		if slices.Contains(numericSchemaFields, key) {
			result[key] = coerceNumericString(value)
			continue
		}
		// :551-559 数组关键字键
		if slices.Contains(arraySchemaKeys, key) {
			array := coerceIndexedObjectToArray(value)
			if array == nil {
				continue // :554 `drop invalid non-array keyword (e.g. enum: "[MaxDepth]")`
			}
			if schemaArrayOfSchemas[key] {
				out := make([]any, 0, len(array))
				for _, entry := range array {
					out = append(out, stripInvalidSchemaConstructs(entry))
				}
				result[key] = out
			} else {
				result[key] = array
			}
			continue
		}
		// :560-579 schema 槽位键
		if slices.Contains(schemaSlotKeys, key) {
			// :561-565 布尔子 schema 必须保留(`additionalProperties: false` 是合法的
			// "锁死对象"写法; 换成 {} 会静默放开额外字段并诱发模型幻觉参数)。
			if isPlainObject(value) || isArrayValue(value) {
				result[key] = stripInvalidSchemaConstructs(value)
			} else if b, ok := value.(bool); ok {
				result[key] = b
			} else if isSchemaPlaceholder(value) {
				result[key] = map[string]any{}
			} else {
				result[key] = value
			}
			continue
		}
		// :580-584 const
		if key == "const" {
			if isSchemaPlaceholder(value) {
				continue
			}
			result[key] = value
			continue
		}
		// :585-607 properties(逐属性保留布尔)
		if key == "properties" && isPlainObject(value) {
			props := map[string]any{}
			for propName, propSchema := range value.(map[string]any) {
				if isPlainObject(propSchema) || isArrayValue(propSchema) {
					props[propName] = stripInvalidSchemaConstructs(propSchema)
				} else if b, ok := propSchema.(bool); ok {
					props[propName] = b
				} else if isSchemaPlaceholder(propSchema) {
					props[propName] = map[string]any{}
				} else {
					props[propName] = propSchema
				}
			}
			result[key] = props
			continue
		}
		// :608-621 $defs / definitions / patternProperties / dependentSchemas
		if (key == "$defs" || key == "definitions" ||
			key == "patternProperties" || key == "dependentSchemas") && isPlainObject(value) {
			defs := map[string]any{}
			for defName, defSchema := range value.(map[string]any) {
				defs[defName] = stripInvalidSchemaConstructs(defSchema)
			}
			result[key] = defs
			continue
		}
		// :622-623 兜底: 容器递归, 标量原样
		//
		// ★ 原注释 :617-621 点名: 占位符只在"期待 sub-schema 的位置"才换成 {}。
		// 标量注解关键字(description / title / pattern / format)里的占位符
		// **必须保持标量** —— 换成 {} 本身就是非法 draft-2020-12, 会再次触发
		// 这个 sanitizer 想避免的那个 400。
		if isPlainObject(value) || isArrayValue(value) {
			result[key] = stripInvalidSchemaConstructs(value)
		} else {
			result[key] = value
		}
	}
	return result
}

// sanitizeClaudeToolSchema 照抄 schemaCoercion.ts:625-632。
//
// 原注释 :627-631: 明确**故意不组合** coerceSchemaNumericFields ——
// 后者会剥掉合法的 `default` 关键字(那是译码器职责), 在原生/透传路径上
// 会静默改写本应逐字转发的工具 schema。
func sanitizeClaudeToolSchema(schema any) any {
	return stripInvalidSchemaConstructs(schema)
}

// sanitizeClaudeToolSchemas 照抄 schemaCoercion.ts:634-639。
func sanitizeClaudeToolSchemas(tools any) any {
	arr, ok := tools.([]any)
	if !ok {
		return tools
	}
	out := make([]any, 0, len(arr))
	for _, tool := range arr {
		rec, isRec := tool.(map[string]any)
		if !isRec {
			out = append(out, tool)
			continue
		}
		// `if (!isPlainObject(tool) || tool.input_schema === undefined) return tool;`
		schema, hasSchema := rec["input_schema"]
		if !hasSchema {
			out = append(out, tool)
			continue
		}
		merged := shallowCopyRecord(rec)
		merged["input_schema"] = sanitizeClaudeToolSchema(schema)
		out = append(out, merged)
	}
	return out
}

// --- 共用小工具 ---

// shallowCopyRecord 等价于 JS 的 `{ ...rec }`(浅拷贝, 新 map)。
func shallowCopyRecord(rec map[string]any) map[string]any {
	out := make(map[string]any, len(rec))
	for k, v := range rec {
		out[k] = v
	}
	return out
}

// isArrayValue 判定是否为 []any(JS 的 Array.isArray)。
func isArrayValue(v any) bool {
	_, ok := v.([]any)
	return ok
}
