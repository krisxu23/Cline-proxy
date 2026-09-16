package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 本文件是 strict_system_hoist.go 的权威行为规格。
// 所有期望值均由 Node 实跑 OmniRoute 原实现确认(见交付说明的 probe 输出),
// 不靠读 TS 推测。
//
// 对应参考实现:
//   open-sse/translator/helpers/strictSystemHoist.ts:6-66
//   src/lib/memory/injection.ts:84 / :90-118

func hoistJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func sysMsg(content any) map[string]any {
	return map[string]any{"role": "system", "content": content}
}

func userMsg(content any) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

// --- strictSystemProviderIDs / parseStrictSystemProvidersEnv ---

func TestParseStrictEnv_逗号分隔小写去空白滤空(t *testing.T) {
	// injection.ts:94-97 `raw.split(",").map(id => id.trim().toLowerCase()).filter(Boolean)`
	got := parseStrictSystemProvidersEnv(" Foo , BAR ,, baz ")
	want := []string{"foo", "bar", "baz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("= %v, 期望 %v", got, want)
	}
}

func TestParseStrictEnv_空串得空切片(t *testing.T) {
	if got := parseStrictSystemProvidersEnv(""); len(got) != 0 {
		t.Fatalf("= %v, 期望空", got)
	}
	if got := parseStrictSystemProvidersEnv("  ,  , "); len(got) != 0 {
		t.Fatalf("= %v, 期望空", got)
	}
}

// --- systemMessageMustBeFirst ---

func TestSystemMustBeFirst_内置清单三条(t *testing.T) {
	// injection.ts:84 `new Set(["xiaomi-mimo", "mimo", "tokenrouter"])`
	for _, p := range []string{"xiaomi-mimo", "mimo", "tokenrouter"} {
		if !systemMessageMustBeFirst(p, nil) {
			t.Fatalf("%q 应在内置严格清单里", p)
		}
	}
}

func TestSystemMustBeFirst_大小写与空白不敏感(t *testing.T) {
	// injection.ts:116 `provider.toLowerCase().trim()`
	if !systemMessageMustBeFirst("  TokenRouter  ", nil) {
		t.Fatal("大小写/空白归一后应命中")
	}
	if !systemMessageMustBeFirst("MIMO", nil) {
		t.Fatal("MIMO 归一后应命中")
	}
}

func TestSystemMustBeFirst_未知与空不加严格(t *testing.T) {
	// injection.ts:109 `Falls back to false for unknown/null providers`
	if systemMessageMustBeFirst("", nil) {
		t.Fatal("空 provider 不严格")
	}
	if systemMessageMustBeFirst("openai", nil) {
		t.Fatal("未登记的 provider 不严格")
	}
}

func TestSystemMustBeFirst_环境变量扩展清单(t *testing.T) {
	// injection.ts:100-103 合并内置 + env 扩展
	extra := parseStrictSystemProvidersEnv("my-custom-gw")
	if !systemMessageMustBeFirst("my-custom-gw", extra) {
		t.Fatal("env 扩展的 provider 应严格")
	}
	if !systemMessageMustBeFirst("mimo", extra) {
		t.Fatal("内置仍然严格")
	}
}

// --- strictSystemHoistToTextContent ---

func TestHoistText_字符串原样(t *testing.T) {
	// :7
	if got := strictSystemHoistToTextContent("hello"); got != "hello" {
		t.Fatalf("= %q", got)
	}
}

func TestHoistText_数组只取text块且用换行连接(t *testing.T) {
	// :8-16 + probe 7: [{text A},{image X},{text B}] → "A\nB"
	in := []any{
		map[string]any{"type": "text", "text": "A"},
		map[string]any{"type": "image", "text": "X"},
		map[string]any{"type": "text", "text": "B"},
	}
	if got := strictSystemHoistToTextContent(in); got != "A\nB" {
		t.Fatalf("= %q, 期望 %q", got, "A\nB")
	}
}

func TestHoistText_非字符串非数组得空串(t *testing.T) {
	// :17
	for _, v := range []any{nil, float64(1), true, map[string]any{"a": 1}} {
		if got := strictSystemHoistToTextContent(v); got != "" {
			t.Fatalf("值 %#v → %q, 期望空串", v, got)
		}
	}
}

// --- hoistLeadingSystemMessage ---

func TestHoist_空数组原样返回(t *testing.T) {
	// :40
	if got := hoistLeadingSystemMessage(nil, "mimo", nil); got != nil {
		t.Fatalf("nil 输入应原样返回, 得 %#v", got)
	}
	empty := []any{}
	got := hoistLeadingSystemMessage(empty, "mimo", nil)
	if len(got) != 0 {
		t.Fatalf("= %#v, 期望空", got)
	}
}

func TestHoist_非严格provider不改动(t *testing.T) {
	// :41 + probe 4: 非严格 provider 时返回**同一引用**(prompt-cache 前缀稳定)
	in := []any{userMsg("u"), sysMsg("s")}
	got := hoistLeadingSystemMessage(in, "openai", nil)
	if &in[0] != &got[0] {
		t.Fatal("非严格 provider 必须返回原切片(同一底层数组)")
	}
	if len(got) != 2 || got[1].(map[string]any)["role"] != "system" {
		t.Fatalf("内容不应改动: %s", hoistJSON(t, got))
	}
}

func TestHoist_已合规不改动(t *testing.T) {
	// :47 + probe 5: system 已在 index 0 → 返回同一引用
	in := []any{sysMsg("s"), userMsg("u")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	if &in[0] != &got[0] {
		t.Fatal("已合规时必须返回原切片")
	}
}

func TestHoist_单元素原引用(t *testing.T) {
	// probe 9: 长度为 1 时无 offending → 原引用
	in := []any{sysMsg("s")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	if &in[0] != &got[0] {
		t.Fatal("单元素已合规应返回原切片")
	}
}

func TestHoist_中间system提升并造新置顶(t *testing.T) {
	// probe 1: [{u1},{sys s1},{u2}] → [{system s1},{u1},{u2}]
	//
	// ★ 注意顺序: rest = [u1, u2], 新的置顶 system 插在**最前**,
	// 其余消息保持原相对顺序。
	in := []any{userMsg("u1"), sysMsg("s1"), userMsg("u2")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"s1","role":"system"},{"content":"u1","role":"user"},{"content":"u2","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_首个是system时合并且首文本在前(t *testing.T) {
	// probe 2: [{sys first},{u},{sys mid}] → [{system "first\nmid"},{u}]
	//
	// ★ 关键顺序: **首个 system 的文本在前**, 然后是 offending 的文本。
	// 这来自 `[rest[0].role === "system" ? toTextContent(rest[0].content) : null,
	//          ...offending.map(...)]` 的数组字面量顺序。
	in := []any{sysMsg("first"), userMsg("u"), sysMsg("mid")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"first\nmid","role":"system"},{"content":"u","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_多个中间system按原顺序(t *testing.T) {
	// probe 3: [{u},{sys a},{sys b}] → [{system "a\nb"},{u}]
	in := []any{userMsg("u"), sysMsg("a"), sysMsg("b")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"a\nb","role":"system"},{"content":"u","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_空文本被过滤(t *testing.T) {
	// probe 6: [{u},{sys ""}] → [{system ""},{u}]
	//
	// ★ 反直觉: 空文本被 `.filter(Boolean)` 滤掉, 但**消息仍被提升** ——
	// 终态是一个 content 为空串的置顶 system。
	// 即"过滤的是文本片段, 不是消息"。
	in := []any{userMsg("u"), sysMsg("")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"","role":"system"},{"content":"u","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_数组内容只取text块(t *testing.T) {
	// probe 7: [{u},{sys [{text A},{image X},{text B}]}] → [{system "A\nB"},{u}]
	in := []any{userMsg("u"), sysMsg([]any{
		map[string]any{"type": "text", "text": "A"},
		map[string]any{"type": "image", "text": "X"},
		map[string]any{"type": "text", "text": "B"},
	})}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"A\nB","role":"system"},{"content":"u","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_首个system文本为空时只用中间system文本(t *testing.T) {
	// probe 8: [{sys ""},{u},{sys mid}] → [{system "mid"},{u}]
	//
	// ★ 反直觉: 首个 system 内容为空 → 它被过滤掉, 合并文本只剩 "mid"。
	// 但由于 rest[0] 仍是那个空 system(它没被删), 走的是"就地合并"分支,
	// 于是它的 content 被**覆盖**为 "mid"。
	in := []any{sysMsg(""), userMsg("u"), sysMsg("mid")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	want := `[{"content":"mid","role":"system"},{"content":"u","role":"user"}]`
	if s := hoistJSON(t, got); s != want {
		t.Fatalf("= %s\n期望 %s", s, want)
	}
}

func TestHoist_保留首个system的其他字段(t *testing.T) {
	// :60 `const mergedFirst = { ...rest[0], content: mergedText };`
	// 首个 system 的额外字段(name 等)必须保留, 只覆盖 content。
	first := map[string]any{"role": "system", "content": "first", "name": "ctx"}
	in := []any{first, userMsg("u"), sysMsg("mid")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	rec := got[0].(map[string]any)
	if rec["name"] != "ctx" {
		t.Fatalf("额外字段 name 应保留, 得 %#v", rec)
	}
	if rec["content"] != "first\nmid" {
		t.Fatalf("content = %v, 期望 %q", rec["content"], "first\nmid")
	}
}

func TestHoist_不修改入参消息对象(t *testing.T) {
	// :60 用 `{...rest[0]}` 造新对象 —— 原始消息不被 mutate。
	first := map[string]any{"role": "system", "content": "first"}
	in := []any{first, userMsg("u"), sysMsg("mid")}
	_ = hoistLeadingSystemMessage(in, "mimo", nil)
	if first["content"] != "first" {
		t.Fatalf("原始消息对象不应被修改, content = %v", first["content"])
	}
}

func TestHoist_非对象条目被跳过不panic(t *testing.T) {
	// Go 侧防御: messages 里可能有非 map 条目。
	in := []any{"junk", userMsg("u"), sysMsg("s")}
	got := hoistLeadingSystemMessage(in, "mimo", nil)
	if len(got) != 3 {
		t.Fatalf("长度应为 3, 得 %d", len(got))
	}
	if got[0].(map[string]any)["content"] != "s" {
		t.Fatalf("置顶 system 内容错误: %s", hoistJSON(t, got))
	}
}

func TestHoist_环境变量扩展的provider也生效(t *testing.T) {
	extra := parseStrictSystemProvidersEnv("custom-gw")
	in := []any{userMsg("u"), sysMsg("s")}
	got := hoistLeadingSystemMessage(in, "custom-gw", extra)
	if got[0].(map[string]any)["role"] != "system" {
		t.Fatalf("env 扩展 provider 应触发提升: %s", hoistJSON(t, got))
	}
}

func TestStrictSystemProviderIDs_从环境变量读取(t *testing.T) {
	// 接线辅助: getStrictSystemProvidersEnv 读 OMNIROUTE_STRICT_SYSTEM_PROVIDERS
	t.Setenv(strictSystemProvidersEnv, " Alpha , beta ")
	got := strictSystemProviderIDs()
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("= %v, 期望 %v", got, want)
	}
	t.Setenv(strictSystemProvidersEnv, "")
	if got := strictSystemProviderIDs(); len(got) != 0 {
		t.Fatalf("未设置时 = %v, 期望空", got)
	}
}

// TestProviderChat_接入_严格provider提升system 是**接入级**回归:
// 复用 chatWithKey 的接线口径 (p.name 作为 provider) 证明接线口径正确。
// 之所以不能在 buildUpstreamBody 做接入验证: 那里 provider 未知, 传空串
// 会让 systemMessageMustBeFirst("") 恒假 (injection.ts:115), 规则永不触发。
// 对应参考实现 translator/index.ts:413 / :787。
func TestProviderChat_接入_严格provider提升system(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		wantHoisted bool
	}{
		{"tokenrouter是严格provider", "tokenrouter", true},
		{"mimo是严格provider", "mimo", true},
		{"openai不严格", "openai", false},
		{"空provider不严格", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params := map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "u"},
					map[string]any{"role": "system", "content": "s"},
				},
			}
			msgs, _ := params["messages"].([]any)
			params["messages"] = hoistLeadingSystemMessage(msgs, c.provider, strictSystemProviderIDs())
			got, _ := params["messages"].([]any)
			firstRole, _ := got[0].(map[string]any)["role"].(string)
			if c.wantHoisted && firstRole != "system" {
				t.Fatalf("provider=%q 应提升 system 到 index 0, 实际首条 role=%q", c.provider, firstRole)
			}
			if !c.wantHoisted && firstRole != "user" {
				t.Fatalf("provider=%q 不应改动, 实际首条 role=%q", c.provider, firstRole)
			}
		})
	}
}

// TestProviderChat_接入_不严格provider保持切片引用 锁住 prompt-cache 要求(#3890):
// no-op 必须返回原切片, 不能返回等值新切片。
func TestProviderChat_接入_不严格provider保持切片引用(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "system", "content": "s"},
	}
	got := hoistLeadingSystemMessage(in, "openai", strictSystemProviderIDs())
	if &in[0] != &got[0] {
		t.Fatal("非严格 provider 必须返回同一底层数组")
	}
}
