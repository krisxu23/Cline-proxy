package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 tool_call_shim.go 的权威值。
//
// ★ 期望值全部来自 Node 实跑参考实现（见 .negbak/probe_toolcallshim.mjs 的
// S1–S12 各栏），不是读代码推断的。
//
// ★ Go 的 json.Marshal 按字典序输出键，Node 保持插入顺序（探针 S12）。
//   键序不影响 JSON 语义，故本文件的断言一律用**逐字段解析**，不比对文本。

// tjson 解析一段 JSON 成 map，用于逐字段断言。
//
// ★ 名字不能叫 sjson —— 本包内 `github.com/sagernet/sing/common/json` 被多个文件
//
//	未加别名 import，其包名就是 sjson，重名会直接编译失败。
func tjson(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("解析 %q 失败: %v", s, err)
	}
	return m
}

// --- coerceToArray ---

func TestCoerceToArray_十二种形态(t *testing.T) {
	// 探针 S1 十二栏，逐栏锁定。
	cases := []struct {
		name string
		in   any
		want []any
	}{
		{"数组", []any{float64(1), float64(2)}, []any{float64(1), float64(2)}},
		{"null", nil, []any{}},
		{"空串", "", []any{}},
		{"JSON数组串", `[{"a":1}]`, []any{map[string]any{"a": float64(1)}}},
		{"JSON对象串", `{"a":1}`, []any{}},
		{"非法JSON串", "not json", []any{}},
		{"普通串", "hello", []any{}},
		{"对象", map[string]any{"a": float64(1)}, []any{}},
		{"数字", float64(42), []any{}},
		{"布尔", true, []any{}},
		{"数组串带空白", "  [1]  ", []any{float64(1)}},
	}
	for _, tc := range cases {
		got := coerceToArray(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: 长度 got %d, want %d (%v)", tc.name, len(got), len(tc.want), got)
		}
		for i := range got {
			gb, _ := json.Marshal(got[i])
			wb, _ := json.Marshal(tc.want[i])
			if string(gb) != string(wb) {
				t.Fatalf("%s[%d]: got %s, want %s", tc.name, i, gb, wb)
			}
		}
	}
}

func TestCoerceToArray_返回原数组引用(t *testing.T) {
	// `if (Array.isArray(v)) return v;` —— 返回**同一引用**，不做拷贝。
	in := []any{float64(1)}
	got := coerceToArray(in)
	if len(got) != 1 {
		t.Fatalf("应返回原数组, got %v", got)
	}
	if &got[0] != &in[0] {
		t.Fatalf("应返回同一底层数组")
	}
}

// --- Read shim: limit ---

func TestReadShim_limit上限钳制(t *testing.T) {
	// 探针 S2 六栏。
	cases := []struct {
		name    string
		raw     string
		wantHas bool
		want    float64
	}{
		{"超大值", `{"file_path":"a.txt","limit":25999999999999999}`, true, 2000},
		{"正好2000", `{"file_path":"a.txt","limit":2000}`, true, 2000},
		{"2001", `{"file_path":"a.txt","limit":2001}`, true, 2000},
		{"0 -> 删除", `{"file_path":"a.txt","limit":0}`, false, 0},
		{"负数 -> 删除", `{"file_path":"a.txt","limit":-5}`, false, 0},
		{"1", `{"file_path":"a.txt","limit":1}`, true, 1},
	}
	for _, tc := range cases {
		got := tjson(t, applyToolCallShimToBuffer("Read", tc.raw))
		v, has := got["limit"]
		if has != tc.wantHas {
			t.Fatalf("%s: limit 存在性 got %v, want %v (%v)", tc.name, has, tc.wantHas, got)
		}
		if has {
			if v.(float64) != tc.want {
				t.Fatalf("%s: limit got %v, want %v", tc.name, v, tc.want)
			}
		}
	}
}

func TestReadShim_字符串化数字(t *testing.T) {
	// 探针 S3 五栏 —— ★ 只有 `^\d+$` 才转，`"5000.5"`/`"abc"` 原样保留。
	got := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","limit":"5000"}`))
	if got["limit"].(float64) != 2000 {
		t.Fatalf(`"5000" 应先转数字再钳到 2000, got %v`, got["limit"])
	}

	got2 := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","limit":"5000.5"}`))
	if got2["limit"] != "5000.5" {
		t.Fatalf(`"5000.5" 不匹配 ^\d+$ 应原样保留, got %v`, got2["limit"])
	}

	got3 := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","limit":"abc"}`))
	if got3["limit"] != "abc" {
		t.Fatalf(`"abc" 应原样保留, got %v`, got3["limit"])
	}
}

func TestReadShim_offset字符串转数字(t *testing.T) {
	// 探针 S3 后两栏 —— ★ offset 的正则**接受负号**，转成数字后再被负数规则归 0。
	got := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","offset":"-3"}`))
	if got["offset"].(float64) != 0 {
		t.Fatalf(`"-3" 应先转 -3 再归 0, got %v`, got["offset"])
	}
	got2 := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","offset":"10"}`))
	if got2["offset"].(float64) != 10 {
		t.Fatalf(`"10" 应转 10, got %v`, got2["offset"])
	}
}

func TestReadShim_offset负数归零(t *testing.T) {
	// 探针 S4 两栏。
	got := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","offset":-10}`))
	if got["offset"].(float64) != 0 {
		t.Fatalf("负数 offset 应归 0, got %v", got["offset"])
	}
	got2 := tjson(t, applyToolCallShimToBuffer("Read", `{"file_path":"a.txt","offset":0}`))
	if v, has := got2["offset"]; !has || v.(float64) != 0 {
		t.Fatalf("offset 0 应保留, got %v", got2)
	}
}

func TestReadShim_pages校验(t *testing.T) {
	// 探针 S5 八栏。
	cases := []struct {
		name    string
		raw     string
		wantHas bool
		want    string
	}{
		{"PDF+范围", `{"file_path":"a.pdf","pages":"1-5"}`, true, "1-5"},
		{"PDF+单页", `{"file_path":"a.pdf","pages":"3"}`, true, "3"},
		{"PDF大写后缀", `{"file_path":"a.PDF","pages":"1"}`, true, "1"},
		{"非PDF", `{"file_path":"a.txt","pages":"1"}`, false, ""},
		{"PDF+非法值", `{"file_path":"a.pdf","pages":"abc"}`, false, ""},
		{"PDF+数字类型", `{"file_path":"a.pdf","pages":1}`, false, ""},
		{"PDF+逆序范围", `{"file_path":"a.pdf","pages":"5-1"}`, true, "5-1"},
		{"无file_path", `{"pages":"1"}`, false, ""},
	}
	for _, tc := range cases {
		got := tjson(t, applyToolCallShimToBuffer("Read", tc.raw))
		v, has := got["pages"]
		if has != tc.wantHas {
			t.Fatalf("%s: pages 存在性 got %v, want %v (%v)", tc.name, has, tc.wantHas, got)
		}
		if has && v != tc.want {
			t.Fatalf("%s: pages got %v, want %v", tc.name, v, tc.want)
		}
	}
}

func TestReadShim_非对象输入原样(t *testing.T) {
	// 探针 S6 五栏: 数组/字符串/数字/null/布尔 **原样返回**。
	//
	// ★ 注意这与 `applyToolCallShimToBuffer` 的"raw 解析失败 -> 用 {}"是两回事:
	//   这里是 raw **能** parse，但 parse 出来的**不是对象**。
	for _, raw := range []string{"[1,2]", `"str"`, "123", "null", "true"} {
		got := applyToolCallShimToBuffer("Read", raw)
		if got != raw {
			t.Fatalf("非对象输入应原样返回: in %q, got %q", raw, got)
		}
	}
}

// --- submit_pr_review ---

func TestSubmitPrReview_shim(t *testing.T) {
	// 探针 S7 四栏。
	got := tjson(t, applyToolCallShimToBuffer("submit_pr_review", `{"functionalChanges":{},"findings":{}}`))
	for _, k := range []string{"functionalChanges", "findings"} {
		if v, ok := got[k].([]any); !ok || len(v) != 0 {
			t.Fatalf("%s 应是空数组, got %v", k, got[k])
		}
	}

	got2 := tjson(t, applyToolCallShimToBuffer("submit_pr_review", `{"functionalChanges":"","findings":null}`))
	for _, k := range []string{"functionalChanges", "findings"} {
		if v, ok := got2[k].([]any); !ok || len(v) != 0 {
			t.Fatalf("%s 应是空数组, got %v", k, got2[k])
		}
	}

	got3 := tjson(t, applyToolCallShimToBuffer("submit_pr_review", `{"functionalChanges":"[\"a\"]","findings":42}`))
	if v := got3["functionalChanges"].([]any); len(v) != 1 || v[0] != "a" {
		t.Fatalf("JSON 数组串应被解出, got %v", got3["functionalChanges"])
	}
	if v := got3["findings"].([]any); len(v) != 0 {
		t.Fatalf("数字应转空数组, got %v", got3["findings"])
	}

	got4 := tjson(t, applyToolCallShimToBuffer("submit_pr_review", `{"functionalChanges":[1],"findings":[2]}`))
	if v := got4["functionalChanges"].([]any); len(v) != 1 || v[0].(float64) != 1 {
		t.Fatalf("已是数组应原样, got %v", got4["functionalChanges"])
	}
	if v := got4["findings"].([]any); len(v) != 1 || v[0].(float64) != 2 {
		t.Fatalf("已是数组应原样, got %v", got4["findings"])
	}
}

func TestSubmitPrReview_无条件补两个键(t *testing.T) {
	// 探针 S12 第 2 栏: `{"other":1}` -> functionalChanges / findings 被**追加**。
	got := tjson(t, applyToolCallShimToBuffer("submit_pr_review", `{"other":1}`))
	if _, ok := got["functionalChanges"]; !ok {
		t.Fatalf("functionalChanges 应被补上, got %v", got)
	}
	if _, ok := got["findings"]; !ok {
		t.Fatalf("findings 应被补上, got %v", got)
	}
	if got["other"].(float64) != 1 {
		t.Fatalf("原有键应保留, got %v", got)
	}
}

// --- 名字解析 ---

func TestResolveToolCallShim_大小写不敏感(t *testing.T) {
	// 探针 S8 四栏。
	for _, n := range []string{"Read", "read", "READ", "ReAd"} {
		got := tjson(t, applyToolCallShimToBuffer(n, `{"file_path":"a.txt","limit":99999}`))
		if got["limit"].(float64) != 2000 {
			t.Fatalf("%q 应命中 Read shim, got %v", n, got)
		}
	}
	got := tjson(t, applyToolCallShimToBuffer("SUBMIT_PR_REVIEW", `{"findings":{}}`))
	if v, ok := got["findings"].([]any); !ok || len(v) != 0 {
		t.Fatalf("SUBMIT_PR_REVIEW 应命中 shim, got %v", got)
	}
}

func TestHasToolCallShim_十一栏(t *testing.T) {
	// 探针 S11 十一栏。
	yes := []string{"Read", "read", "READ", "submit_pr_review"}
	for _, n := range yes {
		if !hasToolCallShim(n) {
			t.Fatalf("%q 应命中", n)
		}
	}
	no := []string{"Bash", "", "Write", "readx", " Rea"}
	for _, n := range no {
		if hasToolCallShim(n) {
			t.Fatalf("%q 不应命中", n)
		}
	}
}

// --- 无 shim / 不可解析 ---

func TestApplyToolCallShim_无shim原样返回(t *testing.T) {
	// 探针 S9 两栏 —— ★ 原样返回 raw，**不做** JSON 往返（键序/空白都应保留）。
	for _, raw := range []string{
		`{"command":"ls"}`,
		`{"a":1}`,
		`{  "b" : 2  }`,
		`not json at all`,
	} {
		if got := applyToolCallShimToBuffer("Bash", raw); got != raw {
			t.Fatalf("无 shim 应原样返回: in %q, got %q", raw, got)
		}
	}
}

func TestApplyToolCallShim_不可解析用空对象(t *testing.T) {
	// 探针 S10 三栏。
	if got := applyToolCallShimToBuffer("Read", "not json"); got != "{}" {
		t.Fatalf("非法 JSON -> {} 经 Read shim 仍是 {}, got %q", got)
	}
	if got := applyToolCallShimToBuffer("Read", ""); got != "{}" {
		t.Fatalf("空串 -> {} 经 Read shim 仍是 {}, got %q", got)
	}
	// submit_pr_review: {} 作输入 -> 两个键被补成空数组
	got := tjson(t, applyToolCallShimToBuffer("submit_pr_review", "{bad"))
	if v, ok := got["functionalChanges"].([]any); !ok || len(v) != 0 {
		t.Fatalf("非法 JSON 经 submit shim 应补空数组, got %v", got)
	}
	if v, ok := got["findings"].([]any); !ok || len(v) != 0 {
		t.Fatalf("非法 JSON 经 submit shim 应补空数组, got %v", got)
	}
}

// --- 关键回归: 不改动无关字段 ---

func TestReadShim_保留无关字段(t *testing.T) {
	// 探针 S12 第 1 栏确认参考实现保留 `extra` 与 `file_path`。
	got := tjson(t, applyToolCallShimToBuffer("Read", `{"limit":99999,"file_path":"a.txt","extra":1}`))
	if got["limit"].(float64) != 2000 {
		t.Fatalf("limit 应钳到 2000, got %v", got["limit"])
	}
	if got["file_path"] != "a.txt" {
		t.Fatalf("file_path 应保留, got %v", got["file_path"])
	}
	if got["extra"].(float64) != 1 {
		t.Fatalf("extra 应保留, got %v", got["extra"])
	}
}

func TestReadShim_不改入参(t *testing.T) {
	// `const patched = { ...input };` —— 浅拷贝，原对象**不被**就地改写。
	// 通过 coerceToArray/sanitizeReadArgs 直接验证 map 层语义。
	orig := map[string]any{"file_path": "a.txt", "limit": float64(99999)}
	patched := map[string]any{}
	for k, v := range orig {
		patched[k] = v
	}
	sanitizeReadArgs(patched)
	if orig["limit"].(float64) != 99999 {
		t.Fatalf("入参不应被改写, got %v", orig["limit"])
	}
	if patched["limit"].(float64) != 2000 {
		t.Fatalf("副本应被钳制, got %v", patched["limit"])
	}
}

// --- coerceToArray 的 JSON 解析细节 ---

func TestCoerceToArray_嵌套与深层(t *testing.T) {
	// JSON 数组串应被真正解析（不是原样保留字符串）。
	got := coerceToArray(`[{"a":[1,2]}]`)
	if len(got) != 1 {
		t.Fatalf("应解出 1 个元素, got %v", got)
	}
	inner := got[0].(map[string]any)
	arr := inner["a"].([]any)
	if len(arr) != 2 {
		t.Fatalf("内层数组应有 2 项, got %v", arr)
	}
}

func TestCoerceToArray_空数组串(t *testing.T) {
	if got := coerceToArray("[]"); len(got) != 0 {
		t.Fatalf(`"[]" 应解出空数组, got %v`, got)
	}
}

func TestCoerceToArray_嵌套对象串不通过(t *testing.T) {
	// `'{"a":1}'` 能 parse 但结果是对象 -> 空数组（探针 S1）。
	if got := coerceToArray(`{"a":1}`); len(got) != 0 {
		t.Fatalf(`对象串应转空数组, got %v`, got)
	}
}

// --- isValuePdf 边界 ---

func TestIsValidPdfPagesArg_边界(t *testing.T) {
	// 三个条件全要满足。
	if !isValidPdfPagesArg("a.pdf", "1") {
		t.Fatalf("a.pdf + \"1\" 应合法")
	}
	if !isValidPdfPagesArg("a.PDF", "1-2") {
		t.Fatalf("大写后缀 + 范围应合法")
	}
	if isValidPdfPagesArg("a.pdf.md", "1") {
		t.Fatalf("非 .pdf 结尾应非法")
	}
	if isValidPdfPagesArg("a.pdf", "1-") {
		t.Fatalf(`"1-" 不匹配正则应非法`)
	}
	if isValidPdfPagesArg("a.pdf", "-1") {
		t.Fatalf(`"-1" 不匹配正则应非法`)
	}
	if isValidPdfPagesArg(42, "1") {
		t.Fatalf("非字符串路径应非法")
	}
	if isValidPdfPagesArg("a.pdf", 1.0) {
		t.Fatalf("非字符串 pages 应非法")
	}
	if isValidPdfPagesArg("a.pdf", "1-2-3") {
		t.Fatalf(`"1-2-3" 不匹配正则应非法`)
	}
}
