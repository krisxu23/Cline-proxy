package app

import (
	"strings"
	"testing"
)

// 本文件锁定 schemaCoercion.ts:449-453 与 sanitizeToolResultId.ts 的权威行为。
//
// ★ 期望值全部来自「逐字搬运参考实现源码后实跑」（探针 .negbak/probe_toolid.mjs）。

// ---------------------------------------------------------------- sanitizeToolID

func TestSanitizeToolID_确定性净化(t *testing.T) {
	// 探针输出逐一搬运。
	// ★ `"!!!"` → `"___"` 是非空结果, 不触发兜底 —— 这条最容易被误写成兜底。
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "abc"},
		{"abc-def_ghi", "abc-def_ghi"},
		{"call.abc", "call_abc"},
		{"toolu:xyz", "toolu_xyz"},
		{"a#b", "a_b"},
		{"a b", "a_b"},
		{`a/b\c`, "a_b_c"},
		{"toolu_01ABC.def:ghi#jkl", "toolu_01ABC_def_ghi_jkl"},
		{"___", "___"},
		{"---", "---"},
		{"!!!", "___"},
		{"  ", "__"},
		{"中文", "__"},
		{"a.b.c", "a_b_c"},
		{"0", "0"},
		{"-", "-"},
		{"_", "_"},
		{".", "_"},
		{":", "_"},
		{"#", "_"},
		{"aaaaa!!!", "aaaaa___"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := sanitizeToolID(c.in)
			if got != c.want {
				t.Errorf("sanitizeToolID(%q) = %q, 期望 %q", c.in, got, c.want)
			}
		})
	}
}

// 空串 → 兜底随机 id
func TestSanitizeToolID_空串兜底(t *testing.T) {
	got := sanitizeToolID("")
	if !strings.HasPrefix(got, "tool_") {
		t.Errorf("空串应兜底成 tool_ 前缀的随机 id, 得 %q", got)
	}
	// 随机: 两次调用必须不同
	got2 := sanitizeToolID("")
	if got == got2 {
		t.Errorf("兜底 id 不随机(两次相同): %q", got)
	}
}

// ★ 兜底 id 必须满足 Anthropic 的 `^[a-zA-Z0-9_-]+$`
func TestSanitizeToolID_兜底id字符集合法(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := sanitizeToolID("")
		if !isAllToolIDSafe(id) {
			t.Fatalf("兜底 id 含非法字符: %q", id)
		}
	}
}

// ★ 净化后的 id 必须恒满足 `^[a-zA-Z0-9_-]+$` —— 这是本模块存在的全部理由。
// 用一批"从其它 provider 重放"的典型脏 id 验证。
func TestSanitizeToolID_净化结果恒合法(t *testing.T) {
	dirty := []string{
		"call.abc", "toolu:xyz", "a#b", "a b", `a/b\c`,
		"toolu_01ABC.def:ghi#jkl", "a@b", "a$b", "a%b", "a&b",
		"a*b", "a+b", "a=b", "a?b", "a!b", "a'b", `a"b`,
		"a(b)", "a[b]", "a{b}", "a<b>", "a,b", "a;b", "a|b",
		"中文", "emoji🎉", "tab\there", "new\nline",
	}
	for _, d := range dirty {
		t.Run(d, func(t *testing.T) {
			got := sanitizeToolID(d)
			if !isAllToolIDSafe(got) {
				t.Errorf("净化后仍含非法字符: %q -> %q", d, got)
			}
			if got == "" {
				t.Errorf("净化结果为空: %q", d)
			}
		})
	}
}

// ★ 配对稳定性: 同一个原始 id 净化两次必须得到同一个结果(无随机性)。
// 这是 tool_use / tool_result 配对能存活的前提。
func TestSanitizeToolID_对同一输入幂等(t *testing.T) {
	for _, id := range []string{"call.abc", "toolu:xyz", "a#b", "abc-def", "!!!", "中文"} {
		a := sanitizeToolID(id)
		b := sanitizeToolID(id)
		if a != b {
			t.Errorf("非幂等: sanitizeToolID(%q) 两次得 %q / %q", id, a, b)
		}
	}
}

// ★ 两侧改写对称性: assistant 的 tool_use.id 与 tool_result.tool_use_id
// 用同一函数改写后必须仍然相等 —— 否则 400 会从 "invalid id" 变成
// "tool_use ids were found without tool_result blocks"。
func TestSanitizeToolID_两侧对称改写下配对存活(t *testing.T) {
	// 客户端重放带来的脏 id
	rawID := "toolu_01ABC.def:ghi#jkl"
	// assistant 侧
	useID := sanitizeToolID(rawID)
	// tool_result 侧
	resultID, ok := sanitizeToolResultID(rawID)
	if !ok {
		t.Fatal("非 falsy id 不应被丢弃")
	}
	if useID != resultID {
		t.Fatalf("两侧改写不对称, 配对会断裂:\n  tool_use.id      = %q\n  tool_result.id   = %q", useID, resultID)
	}
}

// isAllToolIDSafe 判定是否满足 `^[a-zA-Z0-9_-]+$` 且非空。
func isAllToolIDSafe(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !isToolIDAllowedRune(r) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- sanitizeToolResultID

func TestSanitizeToolResultID(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		want   string
		wantOK bool
	}{
		// 探针权威值
		{"undefined→丢弃", nil, "", false},
		{"null→丢弃", nil, "", false},
		{"0→丢弃", float64(0), "", false},
		{"false→丢弃", false, "", false},
		{"空串→丢弃", "", "", false},
		{"123→\"123\"", float64(123), "123", true},
		{"0.5→\"0_5\"", 0.5, "0_5", true},
		{"abc→abc", "abc", "abc", true},
		{"a.b→a_b", "a.b", "a_b", true},
		{"!!!→___", "!!!", "___", true},
		{"true→\"true\"", true, "true", true},
		{"int 0→丢弃", 0, "", false},
		{"int 123→\"123\"", 123, "123", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := sanitizeToolResultID(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok 得 %v, 期望 %v (in=%#v)", ok, c.wantOK, c.in)
			}
			if ok && got != c.want {
				t.Errorf("得 %q, 期望 %q", got, c.want)
			}
		})
	}
}

// ★★ 原注释点名的反直觉点, 必须显式锁定:
//
//	sanitizeToolId() mints a fresh random id for falsy input, which would otherwise
//	defeat that guard and silently fabricate a tool_result that can never match a
//	tool_use.
//
// 即: falsy id **必须**返回"丢弃", 而不是新造一个 id。
// 如果这里被改坏(误用 sanitizeToolID 兜底), 会静默造出永远配对不上的
// tool_result —— 上游 400, 且没有任何线索指向真正原因。
func TestSanitizeToolResultID_不得为falsy输入新造id(t *testing.T) {
	for _, in := range []any{nil, "", float64(0), 0, false} {
		got, ok := sanitizeToolResultID(in)
		if ok {
			t.Errorf("falsy 输入 %#v 不得产生 id, 却得 %q —— 会静默造出配不上的 tool_result", in, got)
		}
	}
}

// ★ 对畸形类型不崩溃（参考实现对 123/true/{} 会走到 String() 后再 .replace(),
// 其中 {}/[] 走 String() 得到 "[object Object]" / "" 再净化）。
// 我方按"无有效 id"处理, 关键是**不得 panic**。
func TestSanitizeToolResultID_畸形类型不崩溃(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("不得 panic: %v", r)
		}
	}()
	weird := []any{
		map[string]any{"a": 1},
		[]any{1, 2},
		[]string{"x"},
		struct{ X int }{1},
	}
	for _, w := range weird {
		_, _ = sanitizeToolResultID(w)
	}
}

// ---------------------------------------------------------------- 数字 → 字符串

func TestJsNumberToString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{123, "123"},
		{0.5, "0.5"},
		{-7, "-7"},
		{1e20, "1e+20"},
		{0.1, "0.1"},
	}
	for _, c := range cases {
		got := jsNumberToString(c.in)
		if got != c.want {
			t.Errorf("jsNumberToString(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}
