package app

import "log"

// 工具定义清洗 —— 逐字照抄 OmniRoute open-sse/services/toolSchemaSanitizer.ts。
//
// 文件头原注释(说明这个模块为什么存在):
//
//	Sanitize OpenAI-format tool definitions for strict upstream JSON Schema
//	validators (e.g. Moonshot AI behind opencode-go/kimi-k2.6).
//
//	The concrete bug this was written for: ForgeCode emits enum schemas like
//	  { type: "string", enum: ["a", "b", "c", null], nullable: true }
//	for nullable optional fields. Lenient providers (Z.AI / GLM) accept the null
//	entry; Moonshot rejects with
//	  "At path 'properties.X.enum': enum value (<nil>) does not match any type
//	   in [string]"
//	before the request reaches the model.
//
//	The fix is to strip null/undefined from `enum` arrays. Everything else here
//	is defensive hygiene: ensures `parameters` is always a valid object schema,
//	filters `required[]` to keys that exist in `properties`, and normalizes a
//	few other shapes that strict validators tend to reject.
//
// 即: agent 工具(如 ForgeCode / Codex)发出的工具 schema 里带 `null` 枚举值,
// 宽松上游(Z.AI / GLM)接受, 但严格上游(Moonshot / opencode-go kimi)会在**请求
// 到达模型之前**直接拒绝整个请求。表现为工具完全不可用。

const maxSchemaRecursionDepth = 32

func isPlainObject(v any) bool {
	if v == nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

// keepOpaqueObjectSchemasOpen 照抄同名函数: 不透明对象 schema 要显式开放,
// 否则严格校验器会拒绝(空 properties 且未声明 additionalProperties 时补 true)。
func keepOpaqueObjectSchemasOpen(schema map[string]any) {
	if _, has := schema["additionalProperties"]; has {
		return
	}
	properties, hasProps := schema["properties"]
	isObjectSchema := schema["type"] == "object" || isPlainObject(properties)
	if !isObjectSchema {
		return
	}
	if !hasProps {
		schema["properties"] = map[string]any{}
		schema["additionalProperties"] = true
	} else if props, ok := properties.(map[string]any); ok && len(props) == 0 {
		schema["additionalProperties"] = true
	}
}

// sanitizeSchema 照抄同名函数, 逐分支对应:
//   - properties: 递归清洗每个子 schema; boolean 形态保留; 其余归一为空对象
//   - items: 数组(元组)形态强制降级为单 schema(严格上游拒绝元组形态)
//   - anyOf/oneOf/allOf: 递归清洗(严格上游会深入校验)
//   - additionalProperties: schema 形态递归; boolean 形态保留
//   - enum: **核心修复** —— 剔除 null/undefined 条目
//   - required: 只保留字符串条目, 且必须存在于 properties
func sanitizeSchema(value any, depth int) map[string]any {
	if depth > maxSchemaRecursionDepth {
		// 超深子树不再下钻: 返回"放开"语义而不是静默抹空 —— 空对象会被严格
		// 上游当"接受任意输入"放行, 合法的深嵌套 anyOf 语义全丢且无 400。
		log.Printf("[schema-sanitizer] schema depth exceeds %d, truncating subtree to open object schema", maxSchemaRecursionDepth)
		return map[string]any{"type": "object", "additionalProperties": true}
	}
	src, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}

	result := make(map[string]any, len(src))
	for k, v := range src {
		if v == nil {
			continue
		}
		switch k {
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				result[k] = v
				continue
			}
			cleaned := make(map[string]any, len(props))
			for pk, pv := range props {
				switch {
				case isPlainObject(pv):
					cleaned[pk] = sanitizeSchema(pv, depth+1)
				case isBool(pv):
					// JSON Schema 2019 boolean 形态: true(任意) / false(无)。
					// 原注释: Preserve as-is; Moonshot accepts these.
					cleaned[pk] = pv
				default:
					cleaned[pk] = map[string]any{}
				}
			}
			result[k] = cleaned

		case "items":
			// 原注释: Recurse into items if it's a single schema. Tuple-form
			// (array) is valid JSON Schema but rejected by Moonshot; coerce to
			// single schema.
			if arr, ok := v.([]any); ok {
				var first map[string]any
				for _, e := range arr {
					if m, ok := e.(map[string]any); ok {
						first = m
						break
					}
				}
				if first != nil {
					result[k] = sanitizeSchema(first, depth+1)
				} else {
					result[k] = map[string]any{}
				}
			} else if isPlainObject(v) {
				result[k] = sanitizeSchema(v, depth+1)
			}

		case "anyOf", "oneOf", "allOf":
			// 原注释: Moonshot recursively validates inside `anyOf` (confirmed
			// empirically), so we must descend to strip null-in-enum etc.
			if arr, ok := v.([]any); ok {
				out := make([]any, 0, len(arr))
				for _, s := range arr {
					if isPlainObject(s) {
						out = append(out, sanitizeSchema(s, depth+1))
					} else {
						out = append(out, map[string]any{})
					}
				}
				result[k] = out
			}

		case "additionalProperties":
			// 原注释: Moonshot recursively validates the schema form of
			// additionalProperties (confirmed empirically).
			if isPlainObject(v) {
				result[k] = sanitizeSchema(v, depth+1)
			} else if isBool(v) {
				result[k] = v
			}

		case "enum":
			// **核心修复**: 剔除 nullable 可选字段带来的 null/undefined 枚举值。
			// 原注释: The actual fix: strip null/undefined entries that ForgeCode
			// adds for nullable optional fields.
			if arr, ok := v.([]any); ok {
				out := make([]any, 0, len(arr))
				for _, e := range arr {
					if e != nil {
						out = append(out, e)
					}
				}
				result[k] = out
			} else {
				result[k] = v
			}

		case "required":
			if arr, ok := v.([]any); ok {
				out := make([]any, 0, len(arr))
				for _, r := range arr {
					if _, ok := r.(string); ok {
						out = append(out, r)
					}
				}
				result[k] = out
			} else {
				result[k] = v
			}

		default:
			result[k] = v
		}
	}

	// required[] 必须只含 properties 里真实存在的键, 否则严格校验器拒绝。
	if reqArr, ok := result["required"].([]any); ok {
		if props, ok := result["properties"].(map[string]any); ok {
			valid := make(map[string]bool, len(props))
			for k := range props {
				valid[k] = true
			}
			filtered := make([]any, 0, len(reqArr))
			for _, r := range reqArr {
				if s, ok := r.(string); ok && valid[s] {
					filtered = append(filtered, r)
				}
			}
			result["required"] = filtered
		}
	}

	keepOpaqueObjectSchemasOpen(result)
	return result
}

// ensureRootObjectType 照抄同名函数。
//
// 原注释: OpenAI's Responses API strict validator requires the ROOT parameters
// schema to declare `type: "object"` explicitly. Clients like the Codex app emit
// `type: null` (rejected upstream as: schema must be a JSON Schema of
// 'type: "object"', got 'type: null' — issue #6359). sanitizeSchema drops the
// null, so at the root we re-add the mandatory "object". Combinator roots
// (anyOf/oneOf/allOf) are left alone — injecting a sibling `type` would change
// their meaning — and explicit root types are preserved as-is.
//
// 即 Codex 会发 `type: null`, Responses API 严格校验器要求根部显式
// `type: "object"`(OmniRoute issue #6359)。组合子根(anyOf/oneOf/allOf)不能注入
// 同级 type, 那会改变语义。
func ensureRootObjectType(schema map[string]any) {
	if _, has := schema["type"]; has {
		return
	}
	if _, has := schema["anyOf"]; has {
		return
	}
	if _, has := schema["oneOf"]; has {
		return
	}
	if _, has := schema["allOf"]; has {
		return
	}
	schema["type"] = "object"
	if !isPlainObject(schema["properties"]) {
		schema["properties"] = map[string]any{}
		if _, has := schema["additionalProperties"]; !has {
			schema["additionalProperties"] = true
		}
	}
}

// normalizeParameters 照抄同名函数: 非对象/空的 parameters 一律补成
// 合法的空对象 schema, 绝不让它变成 null 传给上游。
func normalizeParameters(parameters any) any {
	if isPlainObject(parameters) {
		sanitized := sanitizeSchema(parameters, 0)
		ensureRootObjectType(sanitized)
		return sanitized
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}
}

// sanitizeOpenAITool 照抄同名函数, 覆盖两种形态:
//   - Chat Completions: { type: "function", function: { name, parameters } }
//   - Responses API:    { type: "function", name, parameters }  (无 function 包裹)
//
// 原注释点明第二种形态的必要性: /v1/responses requests reach chatCore in this
// shape and are only unwrapped later by the request translator, so we have to
// sanitize here too.
func sanitizeOpenAITool(tool any) any {
	t, ok := tool.(map[string]any)
	if !ok {
		return tool
	}
	out := shallowCopyRecord(t)

	if fn, ok := out["function"].(map[string]any); ok {
		f := shallowCopyRecord(fn)
		f["parameters"] = normalizeParameters(f["parameters"])
		out["function"] = f
	} else if out["type"] == "function" {
		out["parameters"] = normalizeParameters(out["parameters"])
	}

	return out
}

// sanitizeOpenAITools 照抄同名函数。
func sanitizeOpenAITools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, sanitizeOpenAITool(t))
	}
	return out
}

// flattenOpenAIToolRootAnyOf 照抄同名函数: 根部 anyOf 会让部分上游无法处理,
// 直接摘掉 anyOf 键(保留其余参数形态)。
func flattenOpenAIToolRootAnyOf(tools any) any {
	arr, ok := tools.([]any)
	if !ok {
		return tools
	}
	out := make([]any, 0, len(arr))
	for _, raw := range arr {
		tool, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}

		next := shallowCopyRecord(tool)

		fn, wrapped := next["function"].(map[string]any)
		var fnCopy map[string]any
		if wrapped {
			fnCopy = shallowCopyRecord(fn)
		} else {
			fnCopy = next
		}

		params, ok := fnCopy["parameters"].(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if _, has := params["anyOf"]; !has {
			out = append(out, raw)
			continue
		}

		newParams := shallowCopyRecord(params)
		delete(newParams, "anyOf")
		fnCopy["parameters"] = newParams
		if wrapped {
			next["function"] = fnCopy
		}
		out = append(out, next)
	}
	return out
}

// isBool 判断 any 是否为 bool(Go 的 type switch 简写)。
func isBool(v any) bool {
	_, ok := v.(bool)
	return ok
}
