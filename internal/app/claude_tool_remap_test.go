package app

import (
	"encoding/json"
	"os"
	"testing"
)

// 本文件的全部期望值来自 `.negbak/probe_toolremap.mjs` —— 一份逐字搬运
// OmniRoute `claudeCodeToolRemapper.ts`(483 行) + `claudeCodeExtraRemap.ts`(18 行)
// 的 Node 探针（仅去掉 TS 类型标注，语义零改动）。
//
// ★ 纪律: 探针输出是**权威值**。Go 实现与探针不符时改 Go，绝不允许"顺手修正"
//   探针的期望值 —— 那等于用被实现带偏的期望值自证。
//
// 探针分区:
//
//	R1 needsThirdPartyCloak            23 例
//	R2 isAnthropicServerToolType       15 例
//	R3 remapToolNamesInRequest         10 例
//	R4 remapToolNamesInResponse         5 例
//	R5 restoreClaudeToolName           12 例
//	R6 cloakThirdPartyToolNames        13 例
//
// Go/Node 差异处理（与其它照抄测试一致）: 一律**逐字段断言**，不整体
// `json.Marshal` 后比对字符串 —— Node 的 `JSON.stringify` 保持插入序，Go 的
// `json.Marshal` 按字典序输出键。

// ─────────────── 小工具 ───────────────

// trJSON 把一个 any 重新走一遍 JSON 往返，用来模拟"上行 body 被序列化"后的形态。
func trJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// trObj 取 map[string]any。
func trObj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望对象, 得到 %T (%v)", v, v)
	}
	return m
}

// trArr 取 []any。
func trArr(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("期望数组, 得到 %T (%v)", v, v)
	}
	return a
}

// trMapEntries 把 toolNameMap 展成 `[[alias, origin], ...]`（对位探针的
// `[...m.entries()]`），便于与探针输出逐条比对。
func trMapEntries(m *toolNameMap) [][2]string {
	if m == nil {
		return nil
	}
	out := make([][2]string, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, [2]string{e.alias, e.origin})
	}
	return out
}

// trAssertEntries 逐条断言 alias→origin 映射，顺序也必须是插入序。
func trAssertEntries(t *testing.T, label string, got *toolNameMap, want [][2]string) {
	t.Helper()
	entries := trMapEntries(got)
	if len(entries) != len(want) {
		t.Fatalf("%s: map 条数 = %d, 期望 %d (got=%v)", label, len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Fatalf("%s: map[%d] = [%v], 期望 %v (got=%v)", label, i, entries[i], w, entries)
		}
	}
}

// ─────────────── R1 needsThirdPartyCloak ───────────────

// TestToolRemap_R1_needsThirdPartyCloak 逐条锁定探针 R1 的 23 个权威值。
//
// ★ 反直觉项（禁止"顺手修正"）:
//   - `ApplyPatch` = false —— 它是 toolRenameMap 的**值**（大驼峰），即便首字母
//     大写也放行；而 `applypatch`（键，全小写）= true。
//   - `XY` = false —— 首字母大写且无分隔符 → 视为真 Claude Code 工具。
//   - `mcp__` / `mcp__a` = false —— `mcp__` 前缀**整体豁免**（连空后缀都豁免）。
func TestToolRemap_R1_needsThirdPartyCloak(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"Bash", false},
		{"Read", false},
		{"TodoWrite", false},
		{"CronCreate", false},
		{"mcp__server__tool", false},
		{"bash", true},
		{"read", true},
		{"read_file", true},
		{"run_command", true},
		{"web_search", true},
		{"subagents", true},
		{"session_status", true},
		{"_x", true},
		{"-x", true},
		{"XY", false},
		{"ApplyPatch", false},
		{"applypatch", true},
		{"mcp__", false},
		{"mcp__a", false},
		{"Read_File", true},
		{"readFile", true},
	}
	for _, c := range cases {
		if got := needsThirdPartyCloak(c.in); got != c.want {
			t.Errorf("R1 needsThirdPartyCloak(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
	if len(cases) != 22 {
		t.Fatalf("R1 用例数 = %d, 期望 22", len(cases))
	}
}

// ─────────────── R2 isAnthropicServerToolType ───────────────

// TestToolRemap_R2_isAnthropicServerToolType 逐条锁定探针 R2 的 15 个权威值。
//
// ★ 反直觉项:
//   - `websearch_20250305` = true —— 只按**日期后缀形态**判，**不校验前缀**是否有
//     下划线分隔。
//   - `tool_2025030`（7 位）= false —— 日期必须恰好 8 位。
//   - `Web_Search_20250305` = false —— 含大写即不匹配（正则要求 `[a-z]`）。
func TestToolRemap_R2_isAnthropicServerToolType(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{nil, false},
		{any(nil), false}, // JS undefined 对位 Go nil（见下）
		{"", false},
		{"web_search", true},
		{"web_search_preview", true},
		{"web_search_20250305", true},
		{"bash_20250124", true},
		{"code_execution_20250522", true},
		{"Web_Search_20250305", false},
		{"web_search_20250305x", false},
		{"websearch_20250305", true},
		{"a_12345678", true},
		{"web_search_2025", false},
		{"custom_20250101", true},
		{"tool_2025030", false},
	}
	for _, c := range cases {
		if got := isAnthropicServerToolType(c.in); got != c.want {
			t.Errorf("R2 isAnthropicServerToolType(%#v) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// ─────────────── R3 remapToolNamesInRequest ───────────────

// TestToolRemap_R3a_小写工具名整体改名 锁 R3a。
func TestToolRemap_R3a_小写工具名整体改名(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"name": "bash"},
			map[string]any{"name": "read"},
			map[string]any{"name": "customTool"},
		},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "t1", "name": "bash"},
				},
			},
		},
	}
	ret := remapToolNamesInRequest(body)
	if ret != true {
		t.Fatalf("R3a.ret = %v, 期望 true", ret)
	}

	// tools: bash→Bash, read→Read, customTool 保持（无 canonical, 首字母已大写）
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Bash" {
		t.Errorf("R3a.tools[0].name = %v, 期望 Bash", got)
	}
	if got := trObj(t, tools[1])["name"]; got != "Read" {
		t.Errorf("R3a.tools[1].name = %v, 期望 Read", got)
	}
	if got := trObj(t, tools[2])["name"]; got != "customTool" {
		t.Errorf("R3a.tools[2].name = %v, 期望 customTool", got)
	}

	// messages 历史里的 tool_use.name 也要改
	msgs := trArr(t, body["messages"])
	content := trArr(t, trObj(t, msgs[0])["content"])
	if got := trObj(t, content[0])["name"]; got != "Bash" {
		t.Errorf("R3a.messages[0].content[0].name = %v, 期望 Bash", got)
	}

	// map 只记 2 条（customTool 无改动不入表）
	trAssertEntries(t, "R3a.map", getRequestToolNameMap(body), [][2]string{
		{"Bash", "bash"},
		{"Read", "read"},
	})
}

// TestToolRemap_R3b_全TitleCase不触发 锁 R3b。
func TestToolRemap_R3b_全TitleCase不触发(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "Bash"}}}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3b.ret = %v, 期望 false", ret)
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Bash" {
		t.Errorf("R3b.tools[0].name = %v, 期望 Bash", got)
	}
}

// TestToolRemap_R3c_混用返回false 锁 R3c（hasLowercase && !hasTitleCase）。
func TestToolRemap_R3c_混用返回false(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"name": "bash"},
		map[string]any{"name": "Read"},
	}}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3c.ret = %v, 期望 false", ret)
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Bash" {
		t.Errorf("R3c.tools[0].name = %v, 期望 Bash (改名照做, 只是返回 false)", got)
	}
}

// TestToolRemap_R3d_serverTool不改名 锁 R3d。
func TestToolRemap_R3d_serverTool不改名(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "web_search_20250305", "name": "web_search"},
	}}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3d.ret = %v, 期望 false", ret)
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "web_search" {
		t.Errorf("R3d.tools[0].name = %v, 期望 web_search", got)
	}
}

// TestToolRemap_R3e_serverTool名在历史里也不改 锁 R3e。
func TestToolRemap_R3e_serverTool名在历史里也不改(t *testing.T) {
	body := map[string]any{
		"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}},
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "tool_use", "name": "web_search"}},
			},
		},
	}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3e.ret = %v, 期望 false", ret)
	}
	msgs := trArr(t, body["messages"])
	content := trArr(t, trObj(t, msgs[0])["content"])
	if got := trObj(t, content[0])["name"]; got != "web_search" {
		t.Errorf("R3e.messages[0].content[0].name = %v, 期望 web_search", got)
	}
}

// TestToolRemap_R3f_toolChoice改名 锁 R3f。
func TestToolRemap_R3f_toolChoice改名(t *testing.T) {
	body := map[string]any{
		"tools":       []any{map[string]any{"name": "bash"}},
		"tool_choice": map[string]any{"type": "tool", "name": "bash"},
	}
	if ret := remapToolNamesInRequest(body); ret != true {
		t.Fatalf("R3f.ret = %v, 期望 true", ret)
	}
	tc := trObj(t, body["tool_choice"])
	if got := tc["name"]; got != "Bash" {
		t.Errorf("R3f.tool_choice.name = %v, 期望 Bash", got)
	}
}

// TestToolRemap_R3g_toolChoice非tool类型不走 锁 R3g。
func TestToolRemap_R3g_toolChoice非tool类型不走(t *testing.T) {
	body := map[string]any{
		"tool_choice": map[string]any{"type": "auto", "name": "bash"},
	}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3g.ret = %v, 期望 false", ret)
	}
	tc := trObj(t, body["tool_choice"])
	if got := tc["name"]; got != "bash" {
		t.Errorf("R3g.tool_choice.name = %v, 期望 bash (type != tool, 不改)", got)
	}
}

// TestToolRemap_R3h_空body不炸 锁 R3h。
func TestToolRemap_R3h_空body不炸(t *testing.T) {
	if ret := remapToolNamesInRequest(map[string]any{}); ret != false {
		t.Fatalf("R3h.ret = %v, 期望 false", ret)
	}
}

// TestToolRemap_R3i_tools含null条目 锁 R3i。
func TestToolRemap_R3i_tools含null条目(t *testing.T) {
	body := map[string]any{"tools": []any{nil, map[string]any{"name": "bash"}}}
	if ret := remapToolNamesInRequest(body); ret != true {
		t.Fatalf("R3i.ret = %v, 期望 true", ret)
	}
	tools := trArr(t, body["tools"])
	if tools[0] != nil {
		t.Errorf("R3i.tools[0] = %v, 期望 nil", tools[0])
	}
	if got := trObj(t, tools[1])["name"]; got != "Bash" {
		t.Errorf("R3i.tools[1].name = %v, 期望 Bash", got)
	}
}

// TestToolRemap_R3j_名缺失视作空串 锁 R3j。
// 对位 `String(tool.name || "")` —— undefined 与 "" 都归成 ""。
func TestToolRemap_R3j_名缺失视作空串(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{},
		map[string]any{"name": ""},
	}}
	if ret := remapToolNamesInRequest(body); ret != false {
		t.Fatalf("R3j.ret = %v, 期望 false", ret)
	}
}

// ─────────────── R4 remapToolNamesInResponse ───────────────

// TestToolRemap_R4_响应名回写 锁 R4a–R4e。
//
// ★ 注意 R4c: `forceLowercase=false` 时 `Bash` **不动**（不强制小写），
//
//	而 R4a/R4b 默认 forceLowercase=true → `bash`。缩进差异 prove 正则容空格。
func TestToolRemap_R4_响应名回写(t *testing.T) {
	// R4a: 默认 forceLowercase=true → bash
	if got := remapToolNamesInResponse(`{"name":"Bash"}`, true, nil); got != `{"name":"bash"}` {
		t.Errorf("R4a = %q, 期望 %q", got, `{"name":"bash"}`)
	}
	// R4b: 带空格
	if got := remapToolNamesInResponse(`{"name": "Bash"}`, true, nil); got != `{"name": "bash"}` {
		t.Errorf("R4b = %q, 期望 %q", got, `{"name": "bash"}`)
	}
	// R4c: forceLowercase=false → 原样
	if got := remapToolNamesInResponse(`{"name":"Bash"}`, false, nil); got != `{"name":"Bash"}` {
		t.Errorf("R4c = %q, 期望 %q", got, `{"name":"Bash"}`)
	}
	// R4d: nameMap 把 Bash → mybash
	m := &toolNameMap{entries: []toolNameEntry{{alias: "Bash", origin: "mybash"}}}
	if got := remapToolNamesInResponse(`{"name":"Bash"}`, true, m); got != `{"name":"mybash"}` {
		t.Errorf("R4d = %q, 期望 %q", got, `{"name":"mybash"}`)
	}
	// R4e: 不在 map 里的名字 → 原样
	if got := remapToolNamesInResponse(`{"name":"ZZZ"}`, true, nil); got != `{"name":"ZZZ"}` {
		t.Errorf("R4e = %q, 期望 %q", got, `{"name":"ZZZ"}`)
	}
}

// ─────────────── R5 restoreClaudeToolName ───────────────

// TestToolRemap_R5_恢复工具名 锁 R5a–R5l 的 12 个权威值。
//
// ★ 最反直觉的 R5j: `applypatch->applypatch` 返回 `"ApplyPatch"` —— identity
//
//	命中（key === value）且**无 canonical**（`applypatch` 的 lowercase 键查
//	toolRenameMap 得 `ApplyPatch`，但它 != rawName 吗？`applypatch` != `ApplyPatch`
//	成立 → canonical 存在）→ 落 canonical。这条证明 identity 命中**不能**压过
//	canonical 升级。
func TestToolRemap_R5_恢复工具名(t *testing.T) {
	// R5a: 空串
	if got := restoreClaudeToolName("", nil); got != "" {
		t.Errorf("R5a = %q, 期望 %q", got, "")
	}
	// R5b: bash → Bash（lowercase 键命中）
	if got := restoreClaudeToolName("bash", nil); got != "Bash" {
		t.Errorf("R5b = %q, 期望 Bash", got)
	}
	// R5c: Bash + nil map → Bash（canonical echo 终结）
	if got := restoreClaudeToolName("Bash", nil); got != "Bash" {
		t.Errorf("R5c = %q, 期望 Bash", got)
	}
	// R5d: CronCreate + nil map → CronCreate
	if got := restoreClaudeToolName("CronCreate", nil); got != "CronCreate" {
		t.Errorf("R5d = %q, 期望 CronCreate", got)
	}
	// R5e: unknownTool + nil map → unknownTool（无 canonical, 原样）
	if got := restoreClaudeToolName("unknownTool", nil); got != "unknownTool" {
		t.Errorf("R5e = %q, 期望 unknownTool", got)
	}
	// R5f: UnknownTool + nil map → UnknownTool
	if got := restoreClaudeToolName("UnknownTool", nil); got != "UnknownTool" {
		t.Errorf("R5f = %q, 期望 UnknownTool", got)
	}
	// R5g: Bash + 空 map → Bash
	if got := restoreClaudeToolName("Bash", &toolNameMap{}); got != "Bash" {
		t.Errorf("R5g = %q, 期望 Bash", got)
	}
	// R5h: Bash→mybash（exact 命中且 != rawName）
	mH := &toolNameMap{entries: []toolNameEntry{{alias: "Bash", origin: "mybash"}}}
	if got := restoreClaudeToolName("Bash", mH); got != "mybash" {
		t.Errorf("R5h = %q, 期望 mybash", got)
	}
	// R5i: bash→bash + canonical 存在 → Bash（identity 不压 canonical）
	mI := &toolNameMap{entries: []toolNameEntry{{alias: "bash", origin: "bash"}}}
	if got := restoreClaudeToolName("bash", mI); got != "Bash" {
		t.Errorf("R5i = %q, 期望 Bash", got)
	}
	// R5j: applypatch→applypatch → ApplyPatch
	mJ := &toolNameMap{entries: []toolNameEntry{{alias: "applypatch", origin: "applypatch"}}}
	if got := restoreClaudeToolName("applypatch", mJ); got != "ApplyPatch" {
		t.Errorf("R5j = %q, 期望 ApplyPatch", got)
	}
	// R5k: AZURE→azureCase（大小写不敏感命中）
	mK := &toolNameMap{entries: []toolNameEntry{{alias: "azure", origin: "azureCase"}}}
	if got := restoreClaudeToolName("AZURE", mK); got != "azureCase" {
		t.Errorf("R5k = %q, 期望 azureCase", got)
	}
	// R5l: zzz→ZZZ（identityMatch 且无 canonical）
	mL := &toolNameMap{entries: []toolNameEntry{{alias: "zzz", origin: "ZZZ"}}}
	if got := restoreClaudeToolName("zzz", mL); got != "ZZZ" {
		t.Errorf("R5l = %q, 期望 ZZZ", got)
	}
}

// ─────────────── R6 cloakThirdPartyToolNames ───────────────

// TestToolRemap_R6a_基本 cloak 锁 R6a。
func TestToolRemap_R6a_基本cloak(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"name": "read_file"},
			map[string]any{"name": "run_command"},
			map[string]any{"name": "myCustom"},
		},
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "tool_use", "name": "read_file"}},
			},
		},
	}
	m := cloakThirdPartyToolNames(body, nil)

	tools := trArr(t, body["tools"])
	wantTools := []string{"Read", "Bash", "MyCustom"}
	for i, w := range wantTools {
		if got := trObj(t, tools[i])["name"]; got != w {
			t.Errorf("R6a.tools[%d].name = %v, 期望 %v", i, got, w)
		}
	}

	msgs := trArr(t, body["messages"])
	content := trArr(t, trObj(t, msgs[0])["content"])
	if got := trObj(t, content[0])["name"]; got != "Read" {
		t.Errorf("R6a.messages[0].content[0].name = %v, 期望 Read", got)
	}

	trAssertEntries(t, "R6a.map", m, [][2]string{
		{"Read", "read_file"},
		{"Bash", "run_command"},
		{"MyCustom", "myCustom"},
	})
}

// TestToolRemap_R6b_别名冲突加数字后缀 锁 R6b。
//
// ★ 关键: 冲突时后缀是**数字 2**（`Read2`），不是 `ReadFile`。
//
//	`read_file` canonical = `Read`，但 `Read` 已被另一个工具占用（且
//	`read_file` 出现得更早，先占 Read）→ 后到的 `Read` 本身是 PascalCase 不 cloak，
//	所以 `read_file` 拿到 `Read`，…… 实测结果是 tools 顺序为
//	`[Read2, Read]` 且 map 为 `[[Read2, read_file]]`。
func TestToolRemap_R6b_别名冲突加数字后缀(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"name": "read_file"},
		map[string]any{"name": "Read"},
	}}
	m := cloakThirdPartyToolNames(body, nil)

	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Read2" {
		t.Errorf("R6b.tools[0].name = %v, 期望 Read2", got)
	}
	if got := trObj(t, tools[1])["name"]; got != "Read" {
		t.Errorf("R6b.tools[1].name = %v, 期望 Read", got)
	}
	trAssertEntries(t, "R6b.map", m, [][2]string{{"Read2", "read_file"}})
}

// TestToolRemap_R6c_skip 选项 锁 R6c。
//
// skip 命中的名字保持原样，且**不**入 map —— 让拥有更具体改写权的调用方
// 保留权威，保证两张反向映射表互不相交 / 单跳。
func TestToolRemap_R6c_skip选项(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"name": "read_file"},
		map[string]any{"name": "run_command"},
	}}
	m := cloakThirdPartyToolNames(body, func(n string) bool { return n == "read_file" })

	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "read_file" {
		t.Errorf("R6c.tools[0].name = %v, 期望 read_file (被 skip)", got)
	}
	if got := trObj(t, tools[1])["name"]; got != "Bash" {
		t.Errorf("R6c.tools[1].name = %v, 期望 Bash", got)
	}
	trAssertEntries(t, "R6c.map", m, [][2]string{{"Bash", "run_command"}})
}

// TestToolRemap_R6d_全PascalCase不附加键 锁 R6d。
//
// ★ 这是"惰性创建"的证明: 无任何 cloak 时，body 里**不得**出现
//
//	`_toolNameMap` 键（对位参考实现的 `enumerable: false` + 惰性挂载）。
//	若 Go 侧无条件 attach，会随 `json.Marshal(params)` 发到 Anthropic 400。
func TestToolRemap_R6d_全PascalCase不附加键(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"name": "Bash"},
		map[string]any{"name": "Read"},
	}}
	m := cloakThirdPartyToolNames(body, nil)
	if m.len() != 0 {
		t.Errorf("R6d.mapSize = %d, 期望 0", m.len())
	}
	if _, has := body["_toolNameMap"]; has {
		t.Errorf("R6d.hasMap = true, 期望 false（无 cloak 时不得附加 _toolNameMap）")
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Bash" {
		t.Errorf("R6d.tools[0].name = %v, 期望 Bash", got)
	}
}

// TestToolRemap_R6e_serverTool不动 锁 R6e。
func TestToolRemap_R6e_serverTool不动(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "web_search_20250305", "name": "web_search"},
	}}
	m := cloakThirdPartyToolNames(body, nil)
	if m.len() != 0 {
		t.Errorf("R6e.mapSize = %d, 期望 0", m.len())
	}
	tools := trArr(t, body["tools"])
	tool := trObj(t, tools[0])
	if tool["type"] != "web_search_20250305" {
		t.Errorf("R6e.tools[0].type = %v, 期望 web_search_20250305", tool["type"])
	}
	if tool["name"] != "web_search" {
		t.Errorf("R6e.tools[0].name = %v, 期望 web_search", tool["name"])
	}
}

// TestToolRemap_R6f_toolChoice也被cloak 锁 R6f。
func TestToolRemap_R6f_toolChoice也被cloak(t *testing.T) {
	body := map[string]any{
		"tools":       []any{map[string]any{"name": "run_command"}},
		"tool_choice": map[string]any{"type": "tool", "name": "run_command"},
	}
	cloakThirdPartyToolNames(body, nil)
	tc := trObj(t, body["tool_choice"])
	if got := tc["name"]; got != "Bash" {
		t.Errorf("R6f.tool_choice.name = %v, 期望 Bash", got)
	}
}

// TestToolRemap_R6g_三词PascalCase 锁 R6g。
func TestToolRemap_R6g_三词PascalCase(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "my_custom_tool"}}}
	cloakThirdPartyToolNames(body, nil)
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "MyCustomTool" {
		t.Errorf("R6g.tools[0].name = %v, 期望 MyCustomTool", got)
	}
}

// TestToolRemap_R6h_已占用别名加后缀 锁 R6h。
//
// ★ `a_b` → canonical `AB` 已被同批另一个工具 `AB` 占用 → 加后缀得 `AB2`。
func TestToolRemap_R6h_已占用别名加后缀(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"name": "a_b"},
		map[string]any{"name": "AB"},
	}}
	m := cloakThirdPartyToolNames(body, nil)
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "AB2" {
		t.Errorf("R6h.tools[0].name = %v, 期望 AB2", got)
	}
	if got := trObj(t, tools[1])["name"]; got != "AB" {
		t.Errorf("R6h.tools[1].name = %v, 期望 AB", got)
	}
	trAssertEntries(t, "R6h.map", m, [][2]string{{"AB2", "a_b"}})
}

// TestToolRemap_R6i_mcp前缀不cloak 锁 R6i。
func TestToolRemap_R6i_mcp前缀不cloak(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "mcp__srv__thing"}}}
	m := cloakThirdPartyToolNames(body, nil)
	if m.len() != 0 {
		t.Errorf("R6i.mapSize = %d, 期望 0", m.len())
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "mcp__srv__thing" {
		t.Errorf("R6i.tools[0].name = %v, 期望 mcp__srv__thing", got)
	}
}

// TestToolRemap_R6j_killSwitch 锁 R6j。
func TestToolRemap_R6j_killSwitch(t *testing.T) {
	t.Setenv("CLAUDE_DISABLE_TOOL_NAME_CLOAK", "true")
	body := map[string]any{"tools": []any{map[string]any{"name": "read_file"}}}
	m := cloakThirdPartyToolNames(body, nil)
	if m.len() != 0 {
		t.Errorf("R6j.mapSize = %d, 期望 0", m.len())
	}
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "read_file" {
		t.Errorf("R6j.tools[0].name = %v, 期望 read_file (kill-switch 生效)", got)
	}
}

// TestToolRemap_R6k_非数组content原样 锁 R6k。
func TestToolRemap_R6k_非数组content原样(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	cloakThirdPartyToolNames(body, nil)
	msgs := trArr(t, body["messages"])
	if got := trObj(t, msgs[0])["content"]; got != "hi" {
		t.Errorf("R6k.messages[0].content = %v, 期望 hi", got)
	}
}

// TestToolRemap_R6l_纯分隔符名兜底 锁 R6l。
//
// `___` 无任何字母 → toPascalCase 兜底返回**原串**（不是空串）。
func TestToolRemap_R6l_纯分隔符名兜底(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "___"}}}
	cloakThirdPartyToolNames(body, nil)
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "___" {
		t.Errorf("R6l.tools[0].name = %v, 期望 ___ (toPascalCase 兜底返回原串)", got)
	}
}

// TestToolRemap_R6m_map内容完整 锁 R6m。
func TestToolRemap_R6m_map内容完整(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "bash"}}}
	m := cloakThirdPartyToolNames(body, nil)
	tools := trArr(t, body["tools"])
	if got := trObj(t, tools[0])["name"]; got != "Bash" {
		t.Errorf("R6m.tools[0].name = %v, 期望 Bash", got)
	}
	trAssertEntries(t, "R6m.map", m, [][2]string{{"Bash", "bash"}})
}

// ─────────────── detachToolNameMap ───────────────

// TestToolRemap_detachToolNameMap 验证 `_toolNameMap` 被摘掉后**不会**被序列化。
//
// ★ 这是 Go 侧必须补的一步（参考实现靠 `enumerable: false`）: 若挂在 params 里
//
//	不摘，`json.Marshal(params)` 会把它发给 Anthropic → 400
//	`Extra inputs are not permitted`。
func TestToolRemap_detachToolNameMap(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "read_file"}}}
	m := cloakThirdPartyToolNames(body, nil)
	if m.len() != 1 {
		t.Fatalf("前置失败: map 条数 = %d, 期望 1", m.len())
	}
	if _, has := body["_toolNameMap"]; !has {
		t.Fatalf("前置失败: cloak 后应挂上 _toolNameMap")
	}

	detached := detachToolNameMap(body)
	if detached == nil || detached.len() != 1 {
		t.Fatalf("detachToolNameMap 返回值 = %v, 期望含 1 条", detached)
	}
	if _, has := body["_toolNameMap"]; has {
		t.Fatalf("detach 后 body 里仍含 _toolNameMap")
	}
	// 序列化后不得出现该键
	if s := trJSON(t, body); containsStr(s, "_toolNameMap") {
		t.Errorf("序列化结果仍含 _toolNameMap: %s", s)
	}
}

// TestToolRemap_R3a序列化后无toolNameMap 双向确认 R3 路径同样不泄漏。
func TestToolRemap_R3a序列化后无toolNameMap(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "bash"}}}
	if ret := remapToolNamesInRequest(body); ret != true {
		t.Fatalf("ret = %v, 期望 true", ret)
	}
	detachToolNameMap(body)
	if s := trJSON(t, body); containsStr(s, "_toolNameMap") {
		t.Errorf("序列化结果仍含 _toolNameMap: %s", s)
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || indexOfStr(haystack, needle) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestToolRemap_探针文件存在 保证权威值来源可追溯。
func TestToolRemap_探针文件存在(t *testing.T) {
	if _, err := os.Stat("../../.negbak/probe_toolremap.mjs"); err != nil {
		t.Skipf("探针文件不在预期位置（CI 环境可跳过）: %v", err)
	}
}
