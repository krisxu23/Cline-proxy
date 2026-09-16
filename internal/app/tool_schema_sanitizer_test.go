package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 本测试逐条对应 OmniRoute 参考用例
// tests/unit/tool-schema-sanitizer.test.mjs (517 行, 约 30 个用例),
// 覆盖 open-sse/services/toolSchemaSanitizer.ts 的全部导出函数。
//
// 为什么这是最高优先级: 工具 schema 一旦被严格上游拒收, **整个请求在到达模型
// 之前就失败** —— 表现为 agent 工具完全不可用(不是"偶尔出错", 是一次都用不了)。

// j 把 JSON 字面量解成 any, 便于按参考用例的原样书写测试数据。
func j(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic("测试数据 JSON 非法: " + s + " err=" + err.Error())
	}
	return v
}

// getPath 按路径取嵌套值, 路径元素为字符串(键名)或 int(数组下标)。
func getPath(t *testing.T, root any, path ...any) any {
	t.Helper()
	cur := root
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("路径 %v: 期望 map, 实得 %T", path, cur)
			}
			cur = m[key]
		case int:
			arr, ok := cur.([]any)
			if !ok {
				t.Fatalf("路径 %v: 期望数组, 实得 %T", path, cur)
			}
			if key >= len(arr) {
				t.Fatalf("路径 %v: 下标 %d 越界(长度 %d)", path, key, len(arr))
			}
			cur = arr[key]
		}
	}
	return cur
}

// assertPath 断言路径上的值深度等于期望(与参考用例的 deepEqual 对应)。
func assertPath(t *testing.T, root any, expect any, path ...any) {
	t.Helper()
	got := getPath(t, root, path...)
	if !reflect.DeepEqual(got, expect) {
		gb, _ := json.Marshal(got)
		eb, _ := json.Marshal(expect)
		t.Fatalf("路径 %v 不匹配:\n  实得 %s\n  期望 %s", path, gb, eb)
	}
}

// ───────────────── 核心修复: enum 剔除 null ─────────────────
// 参考用例第 27 行: `assert.deepEqual(out.function.parameters.properties.output_mode.enum, [...])`
// 这是整个模块存在的理由 —— ForgeCode 的 nullable 可选字段会带 null 枚举值,
// Moonshot 在请求到达模型前直接拒绝。

func TestSchema_enum剔除null(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {
			"name": "forge",
			"parameters": {
				"type": "object",
				"properties": {
					"output_mode": {"type": "string", "enum": ["a", "b", null], "nullable": true}
				}
			}
		}
	}`))
	assertPath(t, out, []any{"a", "b"}, "function", "parameters", "properties", "output_mode", "enum")
}

// 参考用例第 46 行: null 在中间也要剔除, 顺序保持。
func TestSchema_enum剔除中间null(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"mode": {"enum": ["a", null, "b"]}}
		}}
	}`))
	assertPath(t, out, []any{"a", "b"}, "function", "parameters", "properties", "mode", "enum")
}

// 参考用例第 61 行: 没有 null 的 enum 原样保留(不得误删值)。
func TestSchema_enum无null时原样保留(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"mode": {"enum": ["a", "b", "c"]}}
		}}
	}`))
	assertPath(t, out, []any{"a", "b", "c"}, "function", "parameters", "properties", "mode", "enum")
}

// ───────────────── required 过滤 ─────────────────
// 参考用例第 79-96 行: required 只保留 properties 里真实存在的键。

func TestSchema_required过滤不存在的键(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"a": {"type": "string"}},
			"required": ["a", "nonexistent"]
		}}
	}`))
	assertPath(t, out, []any{"a"}, "function", "parameters", "required")
}

// required 里的非字符串条目要剔除。
func TestSchema_required剔除非字符串(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"a": {"type": "string"}},
			"required": ["a", 123, null]
		}}
	}`))
	assertPath(t, out, []any{"a"}, "function", "parameters", "required")
}

// ───────────────── items 形态 ─────────────────
// 参考用例第 115-151 行。

func TestSchema_items单schema递归(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"list": {
				"type": "array",
				"items": {"type": "string", "enum": ["x", null]}
			}}
		}}
	}`))
	assertPath(t, out, []any{"x"}, "function", "parameters", "properties", "list", "items", "enum")
}

// items 为元组(数组)形态时降级为单 schema —— 严格上游拒绝元组形态。
func TestSchema_items元组形态降级(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"list": {"type": "array", "items": [{"type": "string"}]}}
		}}
	}`))
	assertPath(t, out, map[string]any{"type": "string"},
		"function", "parameters", "properties", "list", "items")
}

// items 为元组且无对象元素 → 归一为空 schema。
func TestSchema_items元组无对象元素(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"list": {"type": "array", "items": []}}
		}}
	}`))
	assertPath(t, out, map[string]any{}, "function", "parameters", "properties", "list", "items")
}

// ───────────────── anyOf / oneOf / allOf 递归 ─────────────────
// 参考用例第 241-296 行。原注释: Moonshot recursively validates inside anyOf
// (confirmed empirically), so we must descend to strip null-in-enum etc.

func TestSchema_anyOf递归清洗(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"mode": {"anyOf": [{"enum": ["a", "b", null]}]}}
		}}
	}`))
	assertPath(t, out, []any{"a", "b"},
		"function", "parameters", "properties", "mode", "anyOf", 0, "enum")
}

func TestSchema_oneOf递归清洗(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"mode": {"oneOf": [{"enum": ["a", null]}]}}
		}}
	}`))
	assertPath(t, out, []any{"a"},
		"function", "parameters", "properties", "mode", "oneOf", 0, "enum")
}

func TestSchema_allOf递归清洗(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"mode": {"allOf": [{"enum": ["a", null]}]}}
		}}
	}`))
	assertPath(t, out, []any{"a"},
		"function", "parameters", "properties", "mode", "allOf", 0, "enum")
}

// ───────────────── additionalProperties ─────────────────
// 参考用例第 323-340 行。

func TestSchema_additionalProperties_schema形态递归(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"map": {
				"type": "object",
				"additionalProperties": {"enum": ["a", null]}
			}}
		}}
	}`))
	assertPath(t, out, []any{"a"},
		"function", "parameters", "properties", "map", "additionalProperties", "enum")
}

func TestSchema_additionalProperties_boolean形态保留(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"map": {"type": "object", "additionalProperties": false}}
		}}
	}`))
	assertPath(t, out, false,
		"function", "parameters", "properties", "map", "additionalProperties")
}

// ───────────────── properties 里的 boolean schema ─────────────────
// 参考用例第 357-358 行: JSON Schema 2019 boolean 形态要保留。

func TestSchema_properties的boolean形态保留(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"allow_anything": true, "deny_anything": false}
		}}
	}`))
	assertPath(t, out, true, "function", "parameters", "properties", "allow_anything")
	assertPath(t, out, false, "function", "parameters", "properties", "deny_anything")
}

// properties 里非对象非布尔的条目 → 归一为空 schema。
//
// 这里有一个容易搞错的语义层次, 用 Node 实测过才敢写死:
//
//   - 外层循环(toolSchemaSanitizer.ts:51-52)有 null 守卫:
//     `if (v === null || v === undefined) continue;`
//     所以 **schema 自身某个键** 为 null 时, 该键直接消失。
//   - 但 properties 的 **内层循环**(同文件 56-66 行)没有 null 守卫:
//     ```js
//     for (const [pk, pv] of Object.entries(v)) {
//     if (isPlainObject(pv)) { cleaned[pk] = sanitizeSchema(pv, depth + 1); }
//     else if (typeof pv === "boolean") { cleaned[pk] = pv; }
//     else { cleaned[pk] = {}; }
//     }
//     ```
//     `typeof null === "object"` 但不满足 isPlainObject, 于是落进 else 分支,
//     变成 `{}`。Node 实测输出 `{"a":{},"b":{},"c":{}}`。
//
// 结论: properties 里的 null 条目**保留**为 `{}`, 不是消失。二者不可混淆。
func TestSchema_properties非法条目归一为空对象(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object",
			"properties": {"a": "not-a-schema", "b": 42, "c": null}
		}}
	}`))
	assertPath(t, out, map[string]any{}, "function", "parameters", "properties", "a")
	assertPath(t, out, map[string]any{}, "function", "parameters", "properties", "b")
	// c 的 null 落进内层 else 分支 → 归一为空 schema, 键依然存在
	assertPath(t, out, map[string]any{}, "function", "parameters", "properties", "c")
}

// 与外层 null 守卫对照: schema 自身键为 null → 该键彻底消失。
// 用于锁住上面注释里区分的两个语义层次, 防止后人合并成一条规则。
func TestSchema_外层null键被跳过(t *testing.T) {
	out := sanitizeSchema(map[string]any{
		"type":        "object",
		"description": nil,
		"properties":  map[string]any{"a": map[string]any{"type": "string"}},
	}, 0)
	if _, has := out["description"]; has {
		t.Fatal("schema 自身的 null 键应被外层 continue 跳过")
	}
	if _, has := out["properties"]; !has {
		t.Fatal("properties 不应被影响")
	}
}

// ───────────────── Responses API 形态(无 function 包裹) ─────────────────
// 参考用例第 399-406 行。原注释: /v1/responses requests reach chatCore in this
// shape and are only unwrapped later by the request translator.

func TestSchema_Responses形态_无function包裹(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"name": "fs_search",
		"parameters": {
			"type": "object",
			"properties": {"output_mode": {"type": "string", "enum": ["a", "b", null]}}
		}
	}`))
	assertPath(t, out, "fs_search", "name")
	assertPath(t, out, []any{"a", "b"}, "parameters", "properties", "output_mode", "enum")
}

// Responses 形态 parameters 为 null → 补成合法空对象 schema。
func TestSchema_Responses形态_parameters为null(t *testing.T) {
	out := sanitizeOpenAITool(j(`{"type":"function","name":"t","parameters":null}`))
	assertPath(t, out, map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}, "parameters")
}

// ───────────────── 非工具对象原样返回 ─────────────────
// 参考用例第 425 行: { type: "web_search" } 应原样返回。

func TestSchema_非function工具原样返回(t *testing.T) {
	in := j(`{"type": "web_search"}`)
	out := sanitizeOpenAITool(in)
	assertPath(t, out, "web_search", "type")
	if _, has := getPath(t, out).(map[string]any)["function"]; has {
		t.Fatal("非 function 工具不应被注入 function 字段")
	}
}

// ───────────────── 根部 type 补 "object" ─────────────────
// 参考用例第 474-506 行, 对应 Codex 发 type:null 的 issue #6359。

func TestSchema_根部缺type时补object(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"properties": {"schedule": {"type": "string"}}
		}}
	}`))
	assertPath(t, out, "object", "function", "parameters", "type")
	assertPath(t, out, map[string]any{"type": "string"},
		"function", "parameters", "properties", "schedule")
}

func TestSchema_根部type为null时补object(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {"type": null, "properties": {}}}
	}`))
	assertPath(t, out, "object", "function", "parameters", "type")
}

// 组合子根部**不得**注入 type —— 会改变语义。
// 参考用例第 506 行: `assert.equal(out.function.parameters.type, undefined)`
func TestSchema_组合子根部不注入type(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"anyOf": [{"type": "object", "properties": {}}]
		}}
	}`))
	params := getPath(t, out, "function", "parameters").(map[string]any)
	if _, has := params["type"]; has {
		t.Fatalf("anyOf 根部不得注入 type(会改变组合子语义), 实得 %#v", params["type"])
	}
}

// 显式根部 type 原样保留(不得覆盖)。
func TestSchema_显式根部type保留(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {"type": "array", "items": {"type": "string"}}}
	}`))
	assertPath(t, out, "array", "function", "parameters", "type")
}

// ───────────────── 空 properties 开放 additionalProperties ─────────────────
func TestSchema_空properties开放additionalProperties(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {"type": "object", "properties": {}}}
	}`))
	assertPath(t, out, true, "function", "parameters", "additionalProperties")
}

// 已显式声明 additionalProperties 时不得覆盖。
func TestSchema_已声明additionalProperties不覆盖(t *testing.T) {
	out := sanitizeOpenAITool(j(`{
		"type": "function",
		"function": {"name": "t", "parameters": {
			"type": "object", "properties": {}, "additionalProperties": false
		}}
	}`))
	assertPath(t, out, false, "function", "parameters", "additionalProperties")
}

// ───────────────── sanitizeOpenAITools 批量 ─────────────────
// 参考用例第 454-455 行。

func TestSanitizeOpenAITools_批量(t *testing.T) {
	out := sanitizeOpenAITools([]any{
		j(`{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{"m":{"enum":["x",null]}}}}}`),
		j(`{"type":"function","function":{"name":"b","parameters":{"type":"object","properties":{"n":{"type":"string"}}}}}`),
	})
	assertPath(t, out, []any{"x"}, 0, "function", "parameters", "properties", "m", "enum")
	assertPath(t, out, map[string]any{"type": "string"}, 1, "function", "parameters", "properties", "n")
}

// ───────────────── flattenOpenAIToolRootAnyOf ─────────────────
// 根部 anyOf 会让部分上游无法处理, 直接摘掉。

func TestFlattenToolRootAnyOf_摘掉根部anyOf(t *testing.T) {
	out := flattenOpenAIToolRootAnyOf([]any{
		j(`{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"type":"object"}],"type":"object"}}}`),
	})
	params := getPath(t, out, 0, "function", "parameters").(map[string]any)
	if _, has := params["anyOf"]; has {
		t.Fatalf("根部 anyOf 应被摘掉, 实得 %#v", params)
	}
	if params["type"] != "object" {
		t.Fatalf("其余参数形态应保留, 实得 type=%#v", params["type"])
	}
}

// 无 anyOf 的工具原样返回(不得改动)。
func TestFlattenToolRootAnyOf_无anyOf原样返回(t *testing.T) {
	in := j(`{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}`)
	out := flattenOpenAIToolRootAnyOf([]any{in})
	assertPath(t, out, map[string]any{"type": "string"},
		0, "function", "parameters", "properties", "a")
}

// 非数组输入原样返回。
func TestFlattenToolRootAnyOf_非数组原样返回(t *testing.T) {
	in := j(`{"a":1}`)
	if got := flattenOpenAIToolRootAnyOf(in); !reflect.DeepEqual(got, in) {
		t.Fatalf("非数组输入应原样返回, 实得 %#v", got)
	}
}

// ───────────────── 原始输入不得被就地改动 ─────────────────
// 清洗必须产出新对象, 否则会把"已清洗"的 schema 污染到缓存/复用的请求对象上,
// 导致同一请求重试时反复清洗(虽幂等, 但会掩盖原始数据)。

func TestSanitize_不就地改动原始输入(t *testing.T) {
	raw := `{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{"m":{"enum":["a",null]}}}}}`
	in := j(raw)
	before, _ := json.Marshal(in)
	_ = sanitizeOpenAITool(in)
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatalf("sanitizeOpenAITool 不得就地改动输入:\n  前 %s\n  后 %s", before, after)
	}
}
