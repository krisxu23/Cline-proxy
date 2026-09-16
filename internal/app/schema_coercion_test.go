package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是 schema_coercion.go 的权威行为规格。
// 所有期望值均由 Node 实跑 OmniRoute 原实现确认(probe 输出见交付说明),
// 不靠读 TS 推测。对应 schemaCoercion.ts:187-223 / :534-639。

func scJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---------- stripUnsupportedRegexPatterns ----------

func TestStripRegex_前瞻模式被剥离(t *testing.T) {
	// probe A1: {type:"string",pattern:"^(?=.*@).+$"} → {"type":"string"}
	//
	// 这是本模块最核心的用户价值: OpenAI/Codex 的 Responses API 会以
	// "Invalid JSON schema: regex lookaround is not supported." 400 拒绝。
	in := map[string]any{"type": "string", "pattern": "^(?=.*@).+$"}
	got := stripUnsupportedRegexPatterns(in)
	if _, exists := got.(map[string]any)["pattern"]; exists {
		t.Fatalf("前瞻 pattern 应被剥离, 实际 %s", scJSON(t, got))
	}
	if got.(map[string]any)["type"] != "string" {
		t.Fatal("其余字段应保留")
	}
}

func TestStripRegex_后顾模式被剥离(t *testing.T) {
	// probe A2: "(?<=x)y" → pattern 被剥离
	got := stripUnsupportedRegexPatterns(map[string]any{"type": "string", "pattern": "(?<=x)y"})
	if _, exists := got.(map[string]any)["pattern"]; exists {
		t.Fatal("后顾 pattern 应被剥离")
	}
}

func TestStripRegex_负向前瞻与负向后顾(t *testing.T) {
	// REGEX_LOOKAROUND_PATTERN `/\(\?<?[=!]/` 覆盖四种: (?= (?! (?<= (?<!
	for _, p := range []string{"(?=a)", "(?!a)", "(?<=a)", "(?<!a)"} {
		got := stripUnsupportedRegexPatterns(map[string]any{"pattern": p})
		if _, exists := got.(map[string]any)["pattern"]; exists {
			t.Fatalf("pattern %q 应被剥离", p)
		}
	}
}

func TestStripRegex_普通模式保留(t *testing.T) {
	// probe A3: "^[a-z]+$" → 保留
	got := stripUnsupportedRegexPatterns(map[string]any{"type": "string", "pattern": "^[a-z]+$"})
	want := `{"pattern":"^[a-z]+$","type":"string"}`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripRegex_嵌套properties(t *testing.T) {
	// probe A4: 只剥 email 的 pattern, name 不受影响
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"email": map[string]any{"type": "string", "pattern": "^(?=.*@).+$"},
			"name":  map[string]any{"type": "string"},
		},
	}
	got := stripUnsupportedRegexPatterns(in)
	props := got.(map[string]any)["properties"].(map[string]any)
	if _, exists := props["email"].(map[string]any)["pattern"]; exists {
		t.Fatal("email 的 pattern 应被剥离")
	}
	if props["name"].(map[string]any)["type"] != "string" {
		t.Fatal("name 应原样")
	}
}

func TestStripRegex_anyOf数组(t *testing.T) {
	// probe A5: {anyOf:[{pattern:"(?=a)"},{pattern:"ok"}]} → {"anyOf":[{},{"pattern":"ok"}]}
	//
	// ★ 注意第一个条目变成空对象 `{}` 而不是被删掉 —— 因为剥的是
	// **条目内的 pattern 键**, 条目本身仍在数组里。空 schema `{}` 在
	// JSON Schema 里是合法的(表示"任意")。
	in := map[string]any{"anyOf": []any{
		map[string]any{"pattern": "(?=a)"},
		map[string]any{"pattern": "ok"},
	}}
	got := stripUnsupportedRegexPatterns(in)
	arr := got.(map[string]any)["anyOf"].([]any)
	if len(arr) != 2 {
		t.Fatalf("条目数应为 2, 得 %d", len(arr))
	}
	if len(arr[0].(map[string]any)) != 0 {
		t.Fatalf("首个条目应为空对象, 得 %s", scJSON(t, arr[0]))
	}
	if arr[1].(map[string]any)["pattern"] != "ok" {
		t.Fatal("第二个条目应保留 pattern")
	}
}

func TestStripRegex_items递归(t *testing.T) {
	// probe A6: {items:{pattern:"(?!x)"}} → {items:{}}
	in := map[string]any{"type": "array", "items": map[string]any{"pattern": "(?!x)"}}
	got := stripUnsupportedRegexPatterns(in)
	items := got.(map[string]any)["items"].(map[string]any)
	if len(items) != 0 {
		t.Fatalf("items 内的 pattern 应被剥离, 得 %s", scJSON(t, items))
	}
}

func TestStripRegex_布尔additionalProperties不递归(t *testing.T) {
	// :216 `if (result.additionalProperties && typeof result.additionalProperties === "object")`
	// 布尔值不满足, 原样保留。
	in := map[string]any{"type": "object", "additionalProperties": false}
	got := stripUnsupportedRegexPatterns(in)
	if got.(map[string]any)["additionalProperties"] != false {
		t.Fatal("布尔 additionalProperties 应原样保留")
	}
}

func TestStripRegex_非对象非数组原样返回(t *testing.T) {
	// :191
	for _, v := range []any{"str", float64(1), true, nil} {
		if got := stripUnsupportedRegexPatterns(v); got != v {
			t.Fatalf("%#v 应原样返回, 得 %#v", v, got)
		}
	}
}

func TestStripRegex_不修改入参(t *testing.T) {
	// :193 `{ ...schema }` 浅拷贝起步 → 原始对象的 pattern 不被删
	in := map[string]any{"type": "string", "pattern": "(?=a)"}
	_ = stripUnsupportedRegexPatterns(in)
	if _, exists := in["pattern"]; !exists {
		t.Fatal("原对象不应被修改")
	}
}

// ---------- stripInvalidSchemaConstructs ----------

func TestStripInvalid_字符串数字转数值(t *testing.T) {
	// probe B1: {minimum:"5",maxLength:"10"} → {minimum:5,maxLength:10}
	//
	// Anthropic 拒绝字符串形式的数值约束。
	got := stripInvalidSchemaConstructs(map[string]any{"minimum": "5", "maxLength": "10"})
	want := `{"maxLength":10,"minimum":5}`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripInvalid_非数字字符串原样保留(t *testing.T) {
	// probe B2 / B3: "abc" 与 "  " 都保留(trim 后为空也保留)
	got := stripInvalidSchemaConstructs(map[string]any{"minimum": "abc"})
	if got.(map[string]any)["minimum"] != "abc" {
		t.Fatal("非数字字符串应保留")
	}
	got2 := stripInvalidSchemaConstructs(map[string]any{"minimum": "  "})
	if got2.(map[string]any)["minimum"] != "  " {
		t.Fatal("空白字符串应保留")
	}
}

func TestStripInvalid_非数组的数组关键字被丢弃(t *testing.T) {
	// probe B4: {enum:"[MaxDepth]"} → {}
	//
	// :554 `drop invalid non-array keyword (e.g. enum: "[MaxDepth]")`
	got := stripInvalidSchemaConstructs(map[string]any{"enum": "[MaxDepth]"})
	if len(got.(map[string]any)) != 0 {
		t.Fatalf("应被丢弃, 得 %s", scJSON(t, got))
	}
}

func TestStripInvalid_索引对象转数组(t *testing.T) {
	// probe B5: {enum:{"0":"a","1":"b"}} → {enum:["a","b"]}
	got := stripInvalidSchemaConstructs(map[string]any{"enum": map[string]any{"0": "a", "1": "b"}})
	want := `{"enum":["a","b"]}`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripInvalid_非连续索引对象被丢弃(t *testing.T) {
	// coerceIndexedObjectToArray :522 `keys.every((key, index) => String(index) === key)`
	// 键不是 0..n-1 连续 → 返回 null → 丢弃
	got := stripInvalidSchemaConstructs(map[string]any{"enum": map[string]any{"1": "a"}})
	if len(got.(map[string]any)) != 0 {
		t.Fatalf("非连续索引应被丢弃, 得 %s", scJSON(t, got))
	}
}

func TestStripInvalid_占位符在items变空对象(t *testing.T) {
	// probe B6: {items:"[MaxDepth]"} → {items:{}}
	got := stripInvalidSchemaConstructs(map[string]any{"items": "[MaxDepth]"})
	items := got.(map[string]any)["items"].(map[string]any)
	if len(items) != 0 {
		t.Fatalf("占位符应变成空对象, 得 %s", scJSON(t, items))
	}
}

func TestStripInvalid_布尔additionalProperties保留(t *testing.T) {
	// probe B7 —— ★ 最关键的一条反直觉行为
	//
	// :561-565 原注释: "Boolean schemas are valid in JSON Schema (e.g.
	// `additionalProperties: false` locks down the object); coercing to {}
	// would silently allow extras and invite the model to hallucinate arguments."
	got := stripInvalidSchemaConstructs(map[string]any{"type": "object", "additionalProperties": false})
	if got.(map[string]any)["additionalProperties"] != false {
		t.Fatalf("布尔 additionalProperties 必须保留 false, 得 %s", scJSON(t, got))
	}
}

func TestStripInvalid_布尔properties保留(t *testing.T) {
	// probe B8: {properties:{onlyAdmin:false}} → 保留 false
	// :594-596 同一条规则。
	got := stripInvalidSchemaConstructs(map[string]any{
		"properties": map[string]any{"onlyAdmin": false},
	})
	props := got.(map[string]any)["properties"].(map[string]any)
	if props["onlyAdmin"] != false {
		t.Fatalf("布尔子 schema 必须保留, 得 %s", scJSON(t, props))
	}
}

func TestStripInvalid_标量注解里的占位符保留(t *testing.T) {
	// probe B9 —— ★ 第二关键的反直觉行为
	//
	// :617-621 原注释: 占位符只在"期待 sub-schema 的位置"才换成 {}。
	// description / title / pattern / format 里的占位符**必须保持标量**,
	// 换成 {} 本身就是非法 draft-2020-12, 会再次触发该 sanitizer 想避免的 400。
	got := stripInvalidSchemaConstructs(map[string]any{"description": "[MaxDepth]"})
	if got.(map[string]any)["description"] != "[MaxDepth]" {
		t.Fatalf("注解里的占位符必须保持标量, 得 %s", scJSON(t, got))
	}
}

func TestStripInvalid_const占位符被丢弃(t *testing.T) {
	// probe B10: {const:"[MaxDepth]",type:"string"} → {type:"string"}
	got := stripInvalidSchemaConstructs(map[string]any{"const": "[MaxDepth]", "type": "string"})
	if _, exists := got.(map[string]any)["const"]; exists {
		t.Fatalf("const 占位符应被丢弃, 得 %s", scJSON(t, got))
	}
}

func TestStripInvalid_anyOf内递归转换(t *testing.T) {
	// probe B11: {anyOf:[{minimum:"3"},{maximum:"9"}]} → 数值化
	got := stripInvalidSchemaConstructs(map[string]any{"anyOf": []any{
		map[string]any{"minimum": "3"},
		map[string]any{"maximum": "9"},
	}})
	want := `{"anyOf":[{"minimum":3},{"maximum":9}]}`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripInvalid_defs内递归(t *testing.T) {
	// probe B12: {$defs:{A:{minimum:"1"}}} → 数值化
	got := stripInvalidSchemaConstructs(map[string]any{
		"$defs": map[string]any{"A": map[string]any{"minimum": "1"}},
	})
	want := `{"$defs":{"A":{"minimum":1}}}`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripInvalid_数组输入逐项处理(t *testing.T) {
	// probe B13: [{minimum:"1"}] → [{minimum:1}]
	got := stripInvalidSchemaConstructs([]any{map[string]any{"minimum": "1"}})
	want := `[{"minimum":1}]`
	if s := scJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestStripInvalid_占位符字符串输入变空对象(t *testing.T) {
	// probe B14: 输入 "[MaxDepth]" → {}
	got := stripInvalidSchemaConstructs("[MaxDepth]")
	if len(got.(map[string]any)) != 0 {
		t.Fatalf("应变成空对象, 得 %#v", got)
	}
}

func TestStripInvalid_普通字符串输入原样(t *testing.T) {
	// probe B15: "hello" → "hello"
	if got := stripInvalidSchemaConstructs("hello"); got != "hello" {
		t.Fatalf("= %#v, 期望原样", got)
	}
}

func TestStripInvalid_otherScalars原样(t *testing.T) {
	// 兜底分支: 非对象非数组且非占位符 → 原样
	for _, v := range []any{float64(1), true, nil} {
		if got := stripInvalidSchemaConstructs(v); got != v {
			t.Fatalf("%#v 应原样, 得 %#v", v, got)
		}
	}
}

// ---------- sanitizeClaudeToolSchema / sanitizeClaudeToolSchemas ----------

func TestSanitizeClaudeToolSchema_等价于stripInvalid(t *testing.T) {
	// :625-632 原注释明确: **故意不组合** coerceSchemaNumericFields
	// (那个会剥掉合法的 `default` 关键字)。
	// 故 `default` 必须保留。
	in := map[string]any{"type": "object", "default": map[string]any{"a": 1}, "minimum": "2"}
	got := sanitizeClaudeToolSchema(in).(map[string]any)
	if _, exists := got["default"]; !exists {
		t.Fatal("default 关键字必须保留(本函数不组合 coerceSchemaNumericFields)")
	}
	if got["minimum"] != float64(2) {
		t.Fatalf("字符串数字应转换, 得 %#v", got["minimum"])
	}
}

func TestSanitizeClaudeToolSchemas_逐工具处理input_schema(t *testing.T) {
	// :634-639
	in := []any{
		map[string]any{
			"name":         "f",
			"input_schema": map[string]any{"minimum": "5"},
		},
	}
	got := sanitizeClaudeToolSchemas(in).([]any)
	tool := got[0].(map[string]any)
	if tool["name"] != "f" {
		t.Fatal("其他字段应保留")
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["minimum"] != float64(5) {
		t.Fatalf("input_schema 应被清洗, 得 %s", scJSON(t, schema))
	}
}

func TestSanitizeClaudeToolSchemas_无input_schema时原样(t *testing.T) {
	// :637 `if (!isPlainObject(tool) || tool.input_schema === undefined) return tool;`
	in := []any{map[string]any{"name": "f"}}
	got := sanitizeClaudeToolSchemas(in).([]any)
	if _, exists := got[0].(map[string]any)["input_schema"]; exists {
		t.Fatal("不应凭空添加 input_schema")
	}
}

func TestSanitizeClaudeToolSchemas_非数组原样(t *testing.T) {
	// :635 `if (!Array.isArray(tools)) return tools;`
	if got := sanitizeClaudeToolSchemas("nope"); got != "nope" {
		t.Fatalf("= %#v, 期望原样", got)
	}
	if got := sanitizeClaudeToolSchemas(nil); got != nil {
		t.Fatalf("nil 应原样, 得 %#v", got)
	}
}

func TestSanitizeClaudeToolSchemas_不修改入参工具对象(t *testing.T) {
	// :638 `{ ...tool, input_schema: ... }` → 新对象
	orig := map[string]any{"name": "f", "input_schema": map[string]any{"minimum": "5"}}
	_ = sanitizeClaudeToolSchemas([]any{orig})
	if orig["input_schema"].(map[string]any)["minimum"] != "5" {
		t.Fatal("原始工具对象不应被修改")
	}
}

// coerceIndexedObjectToArray 的直接用例。
func TestCoerceIndexed_各类输入(t *testing.T) {
	// 已是数组 → 原样
	arr := []any{"a"}
	if got := coerceIndexedObjectToArray(arr); len(got) != 1 {
		t.Fatal("数组应原样")
	}
	// 连续索引 → 转换
	got := coerceIndexedObjectToArray(map[string]any{"0": "a", "1": "b", "2": "c"})
	if strings.Join([]string{got[0].(string), got[1].(string), got[2].(string)}, "") != "abc" {
		t.Fatalf("= %#v", got)
	}
	// 非对象 / 空对象 / 非连续 → nil
	if got := coerceIndexedObjectToArray("x"); got != nil {
		t.Fatal("非对象应为 nil")
	}
	if got := coerceIndexedObjectToArray(map[string]any{}); got != nil {
		t.Fatal("空对象应为 nil")
	}
	if got := coerceIndexedObjectToArray(map[string]any{"a": 1}); got != nil {
		t.Fatal("非数字键应为 nil")
	}
}

func TestCoerceNumericString_各类输入(t *testing.T) {
	// 非字符串原样
	if got := coerceNumericString(float64(5)); got != float64(5) {
		t.Fatal("数值应原样")
	}
	// 数字字符串 → 数值
	if got := coerceNumericString("42"); got != float64(42) {
		t.Fatalf("= %#v", got)
	}
	// 带空白 → trim 后转换
	if got := coerceNumericString("  7  "); got != float64(7) {
		t.Fatalf("= %#v", got)
	}
	// 小数与负数
	if got := coerceNumericString("-1.5"); got != float64(-1.5) {
		t.Fatalf("= %#v", got)
	}
	// 非数字 / 空串原样
	if got := coerceNumericString("abc"); got != "abc" {
		t.Fatal("非数字应原样")
	}
	if got := coerceNumericString(""); got != "" {
		t.Fatal("空串应原样")
	}
}

func TestHasUnsupportedRegexLookaround_边界(t *testing.T) {
	if hasUnsupportedRegexLookaround("^abc$") {
		t.Fatal("普通正则不应命中")
	}
	if !hasUnsupportedRegexLookaround("(?=x)") {
		t.Fatal("前瞻应命中")
	}
	if hasUnsupportedRegexLookaround(float64(1)) {
		t.Fatal("非字符串应返回 false")
	}
	if hasUnsupportedRegexLookaround(nil) {
		t.Fatal("nil 应返回 false")
	}
	// 非 lookaround 的 `(?` 形式 (非捕获组 / 命名组) 不应命中
	if hasUnsupportedRegexLookaround("(?:abc)") {
		t.Fatal("非捕获组不应命中")
	}
}

// TestStripInvalid_非法非数组关键字被丢弃 照抄 :551-559 的 `continue` 分支。
//
// 负向验证补测: 把该 continue 改成 `if !false { continue }` 后测试**没有变红**,
// 说明此处原先无覆盖 —— 而它正是 Claude Code 发来的 `enum: "[MaxDepth]"` 这类
// 「注释被序列化成字符串」故障的唯一拦截点(原样透传会导致上游 400)。
// 期望值全部由 Node 实跑 schemaCoercion.ts 得到, 非推测。
func TestStripInvalid_非法非数组关键字被丢弃(t *testing.T) {
	// enum 是字符串 → 整个键被丢弃 (Node: {} )
	got := stripInvalidSchemaConstructs(map[string]any{"enum": "[MaxDepth]"})
	if _, exists := got.(map[string]any)["enum"]; exists {
		t.Fatalf("字符串 enum 应被丢弃, 得 %s", scJSON(t, got))
	}
	// required 是字符串 → 丢弃 (Node: {} )
	got = stripInvalidSchemaConstructs(map[string]any{"required": "a,b"})
	if _, exists := got.(map[string]any)["required"]; exists {
		t.Fatalf("字符串 required 应被丢弃, 得 %s", scJSON(t, got))
	}
	// 合法数组 → 保留 (Node: {"enum":["a","b"]} )
	got = stripInvalidSchemaConstructs(map[string]any{"enum": []any{"a", "b"}})
	arr, ok := got.(map[string]any)["enum"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("合法 enum 数组应保留, 得 %s", scJSON(t, got))
	}
}

// TestStripInvalid_索引对象被还原为数组 照抄 coerceIndexedObjectToArray (:519-528)。
// 期望值由 Node 实跑得到: {"enum":{0:"a",1:"b"}} → {"enum":["a","b"]}。
func TestStripInvalid_索引对象被还原为数组(t *testing.T) {
	got := stripInvalidSchemaConstructs(map[string]any{"enum": map[string]any{"0": "a", "1": "b"}})
	arr, ok := got.(map[string]any)["enum"].([]any)
	if !ok || len(arr) != 2 || arr[0] != "a" || arr[1] != "b" {
		t.Fatalf("索引对象应还原为 [a b], 得 %s", scJSON(t, got))
	}
	// type **不在** ARRAY_SCHEMA_KEYS 里 (:504 只有 enum/required/anyOf/oneOf/allOf/prefixItems),
	// 因此 type 的索引对象**原样保留** (Node 实跑: {"type":{"0":"string"}})。
	// 这一条曾被我用"伪造常量"的探针误导过, 必须以真实源码为准。
	got = stripInvalidSchemaConstructs(map[string]any{"type": map[string]any{"0": "string"}})
	inner, ok := got.(map[string]any)["type"].(map[string]any)
	if !ok || inner["0"] != "string" {
		t.Fatalf("type 不是数组关键字, 索引对象应原样保留, 得 %s", scJSON(t, got))
	}
	// anyOf 是数组关键字且是 array-of-schemas → 索引对象还原并**逐项递归**
	// (Node: {"anyOf":[{"type":"string"},{"type":"number"}]})
	got = stripInvalidSchemaConstructs(map[string]any{
		"anyOf": map[string]any{"0": map[string]any{"type": "string"}, "1": map[string]any{"type": "number"}},
	})
	arr, ok = got.(map[string]any)["anyOf"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("anyOf 索引对象应还原为长度 2 的数组, 得 %s", scJSON(t, got))
	}
	// 键不是 0,1,2... 连续下标 → 不是数组, 整键丢弃 (coerceIndexedObjectToArray 返回 nil)
	got = stripInvalidSchemaConstructs(map[string]any{"enum": map[string]any{"x": "a"}})
	if _, exists := got.(map[string]any)["enum"]; exists {
		t.Fatalf("非连续下标对象应整键丢弃, 得 %s", scJSON(t, got))
	}
	// 空对象 → 视为 nil → 丢弃
	got = stripInvalidSchemaConstructs(map[string]any{"enum": map[string]any{}})
	if _, exists := got.(map[string]any)["enum"]; exists {
		t.Fatalf("空对象 enum 应被丢弃, 得 %s", scJSON(t, got))
	}
}

// TestStripInvalid_数值字符串被就地转换 照抄 coerceNumericString (:500-517) 在
// stripInvalidSchemaConstructs 里的调用点 (:547-550)。
// Node 实跑: {"minLength":"5"} → {"minLength":5}。
func TestStripInvalid_数值字符串被就地转换(t *testing.T) {
	got := stripInvalidSchemaConstructs(map[string]any{"minLength": "5"})
	if v, ok := got.(map[string]any)["minLength"].(float64); !ok || v != 5 {
		t.Fatalf("minLength 字符串 \"5\" 应转数值 5, 得 %s", scJSON(t, got))
	}
	// 非数字字符串原样保留 (coerceNumericString 原样返回)
	got = stripInvalidSchemaConstructs(map[string]any{"maxLength": "many"})
	if got.(map[string]any)["maxLength"] != "many" {
		t.Fatalf("非数字字符串应原样, 得 %s", scJSON(t, got))
	}
}

// TestAnthropicToolsToOpenAI_接入schema净化 是**接入级**测试。
//
// 照抄纪律第 2 条要求: 不仅要证明纯函数正确, 还要证明它长在真实调用链上。
// 走的是真实口径 —— 构造 Anthropic 形态的 tools, 经 anthropicToolsToOpenAI,
// 检查 OpenAI 形态的 function.parameters 已被净化。
//
// 对位参考实现 executors/base.ts:970 / executors/cliproxyapi.ts:347。
func TestAnthropicToolsToOpenAI_接入schema净化(t *testing.T) {
	in := []any{
		map[string]any{
			"name": "Read",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					// ★ Claude Code 截断占位符: 原样透传会被上游拒为
					//   `tools.0.custom.input_schema: JSON schema is invalid`
					"maxDepth": map[string]any{"enum": "[MaxDepth]"},
					// ★ lookaround 正则: 多数上游 JSON Schema 实现不支持
					"email": map[string]any{"type": "string", "pattern": "^(?=.*@).+$"},
					// 合法字段必须原样保留
					"limit": map[string]any{"type": "number", "minLength": "5"},
				},
				"additionalProperties": false,
			},
		},
	}
	got := anthropicToolsToOpenAI(in)
	if len(got) != 1 {
		t.Fatalf("应产出 1 个工具, 得 %d", len(got))
	}
	fn := got[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	// enum 字符串占位符被丢弃
	if _, exists := props["maxDepth"].(map[string]any)["enum"]; exists {
		t.Fatalf("enum 占位符应被丢弃, 得 %s", scJSON(t, props["maxDepth"]))
	}
	// lookaround 正则**不被**这条路径剥离 (参考实现只用 stripInvalidSchemaConstructs)
	if props["email"].(map[string]any)["pattern"] != "^(?=.*@).+$" {
		t.Fatalf("本路径不剥离 lookaround (与参考实现一致), 得 %s", scJSON(t, props["email"]))
	}
	// 数值字符串被转换
	if v, ok := props["limit"].(map[string]any)["minLength"].(float64); !ok || v != 5 {
		t.Fatalf("minLength \"5\" 应转 5, 得 %s", scJSON(t, props["limit"]))
	}
	// 布尔 additionalProperties 保留
	if params["additionalProperties"] != false {
		t.Fatalf("additionalProperties 应保留 false, 得 %s", scJSON(t, params))
	}
}

// TestAnthropicToolsToOpenAI_OpenAI形态工具原样透传 锁住 `type=="function"` 的早退分支。
func TestAnthropicToolsToOpenAI_OpenAI形态工具原样透传(t *testing.T) {
	orig := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":       "x",
			"parameters": map[string]any{"enum": "[MaxDepth]"},
		},
	}
	got := anthropicToolsToOpenAI([]any{orig})
	// 该分支是 `out = append(out, t)` 原样透传 —— 不净化。
	// 这是参考实现之外的既有行为, 此处只做**现状锁定**, 不改变它。
	if got[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["enum"] != "[MaxDepth]" {
		t.Fatal("OpenAI 形态工具应原样透传(现状锁定)")
	}
}
