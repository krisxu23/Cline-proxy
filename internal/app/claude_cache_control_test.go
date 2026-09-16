package app

import (
	"testing"
)

// 本文件锁定 claude_cache_control.go 的权威值。
//
// ★ 所有期望值均来自 Node 实跑参考实现 (见 .negbak/probe_cachecontrol.mjs 的
// E1–E6 各栏), 不是读代码推断的。
//
// ★ Go 与 Node 的 JSON 键序不同(Go 按字典序, Node 按插入顺序), 因此涉及
// 多字段对象的断言一律**逐字段**写, 不比对序列化文本。

// --- ensureMessageContentArray ---

func TestEnsureMessageContentArray_三种形态(t *testing.T) {
	// 探针 E1 前三栏 + 后四栏。

	// 数组 -> 原样返回同一引用
	arr := []any{map[string]any{"type": "text", "text": "a"}}
	m1 := map[string]any{"content": arr}
	got := ensureMessageContentArray(m1)
	if len(got) != 1 || &got[0] != &arr[0] {
		t.Fatalf("数组应原样返回同一底层数组, got %v", got)
	}

	// 非空字符串 -> 就地改写成块数组
	m2 := map[string]any{"content": "hi"}
	got2 := ensureMessageContentArray(m2)
	if len(got2) != 1 {
		t.Fatalf("非空字符串应包成 1 块, got %d", len(got2))
	}
	b := mustJSON(t, got2[0])
	if b["type"] != "text" || b["text"] != "hi" {
		t.Fatalf("块应是 {text,hi}, got %v", b)
	}
	// ★ 就地改写: m2["content"] 必须变成块数组
	if _, isArr := m2["content"].([]any); !isArr {
		t.Fatalf("非空字符串应被就地改写为块数组, got %T", m2["content"])
	}

	// 空白串 / 空串 / null / 数字 -> 空切片, 且**不改写**
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"空白串", "   "},
		{"空串", ""},
		{"null", nil},
		{"数字", 42},
	} {
		m := map[string]any{"content": tc.in}
		if got := ensureMessageContentArray(m); len(got) != 0 {
			t.Fatalf("%s 应返回空切片, got %v", tc.name, got)
		}
		if m["content"] != tc.in {
			t.Fatalf("%s 不应被改写, got %v", tc.name, m["content"])
		}
	}
}

func TestEnsureMessageContentArray_content缺失(t *testing.T) {
	// 探针 E1 未直接覆盖, 但参考实现走 `msg?.content` 可选链 -> undefined ->
	// 不是字符串 -> 返回 []。
	m := map[string]any{"role": "user"}
	if got := ensureMessageContentArray(m); len(got) != 0 {
		t.Fatalf("content 缺失应返回空切片, got %v", got)
	}
}

// --- markMessageCacheControl ---

func TestMarkMessageCacheControl_覆盖与ttl(t *testing.T) {
	// 探针 E2 前两栏 + "已有 cc 被覆盖"。

	// 无 ttl -> {type:"ephemeral"}
	a := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b"},
	}}
	if !markMessageCacheControl(a, "", false) {
		t.Fatalf("应返回 true")
	}
	blocks := a["content"].([]any)
	// ★ 只标记**最后一个**块
	if _, has := mustJSON(t, blocks[0])["cache_control"]; has {
		t.Fatalf("第一块不应被标记, got %v", blocks[0])
	}
	cc := mustJSON(t, mustJSON(t, blocks[1])["cache_control"])
	if cc["type"] != "ephemeral" {
		t.Fatalf("标记应为 ephemeral, got %v", cc)
	}
	if _, has := cc["ttl"]; has {
		t.Fatalf("无 ttl 时不应有 ttl 字段, got %v", cc)
	}

	// 带 ttl -> {type:"ephemeral","ttl":"1h"}
	b := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "a"},
	}}
	if !markMessageCacheControl(b, "1h", true) {
		t.Fatalf("应返回 true")
	}
	cc2 := mustJSON(t, mustJSON(t, b["content"].([]any)[0])["cache_control"])
	if cc2["type"] != "ephemeral" || cc2["ttl"] != "1h" {
		t.Fatalf("标记应为 {ephemeral,1h}, got %v", cc2)
	}

	// ★ 覆盖已有 cc(不是保留也不是合并)
	c := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "a", "cache_control": map[string]any{"old": true}},
	}}
	if !markMessageCacheControl(c, "1h", true) {
		t.Fatalf("应返回 true")
	}
	cc3 := mustJSON(t, mustJSON(t, c["content"].([]any)[0])["cache_control"])
	if _, has := cc3["old"]; has {
		t.Fatalf("旧 cc 应被整体替换, got %v", cc3)
	}
	if cc3["ttl"] != "1h" {
		t.Fatalf("新 cc 应带 ttl, got %v", cc3)
	}
}

func TestMarkMessageCacheControl_空content返回false(t *testing.T) {
	// 探针 E2 "空数组" / "空白串" 两栏。
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"空数组", []any{}},
		{"空白串", "   "},
		{"null", nil},
	} {
		m := map[string]any{"role": "user", "content": tc.in}
		if markMessageCacheControl(m, "", false) {
			t.Fatalf("%s 应返回 false", tc.name)
		}
	}
}

func TestMarkMessageCacheControl_字符串content被就地改写(t *testing.T) {
	// 探针 E2 "字符串(非空)" 一栏: content 是 "str" -> 被包成块并挂 cc。
	m := map[string]any{"role": "user", "content": "str"}
	if !markMessageCacheControl(m, "", false) {
		t.Fatalf("应返回 true")
	}
	blocks, ok := m["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content 应被就地改写为 1 块, got %v", m["content"])
	}
	b := mustJSON(t, blocks[0])
	if b["text"] != "str" {
		t.Fatalf("块文本应为 str, got %v", b)
	}
	if _, has := b["cache_control"]; !has {
		t.Fatalf("块应带 cache_control, got %v", b)
	}
}

func TestMarkMessageCacheControl_非对象块返回false不panic(t *testing.T) {
	// 探针 E2 "[null] 块 -> 抛错: Cannot set properties of null"。
	// ★ 有意偏离: 参考实现在此崩溃; 长驻服务改为返回 false + 不 panic。
	m := map[string]any{"role": "user", "content": []any{nil}}
	if markMessageCacheControl(m, "", false) {
		t.Fatalf("非对象块应返回 false")
	}
}

// --- bodyHasAnyCacheControl ---

func TestBodyHasAnyCacheControl_九个判定(t *testing.T) {
	// 探针 E3 全部十栏(逐条对位)。
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"空 body", map[string]any{}, false},
		{"system 带 cc", map[string]any{"system": []any{
			map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}}}, true},
		{"system 无 cc", map[string]any{"system": []any{
			map[string]any{"type": "text", "text": "s"}}}, false},
		{"messages 块带 cc", map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"cache_control": map[string]any{}}}}}}, true},
		{"messages content 是串", map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hi"}}}, false},
		{"tools 带 cc", map[string]any{"tools": []any{
			map[string]any{"name": "T", "cache_control": map[string]any{"type": "ephemeral"}}}}, true},
		{"tools 无 cc", map[string]any{"tools": []any{
			map[string]any{"name": "T"}}}, false},
		{"cc 是 null", map[string]any{"system": []any{
			map[string]any{"cache_control": nil}}}, false},
		{"cc 是空对象", map[string]any{"system": []any{
			map[string]any{"cache_control": map[string]any{}}}}, true},
		{"system 是字符串", map[string]any{"system": "str"}, false},
		{"messages 是 null", map[string]any{"messages": nil}, false},
	}
	for _, tc := range cases {
		if got := bodyHasAnyCacheControl(tc.body); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- reanchorSystemCacheControl ---

func TestReanchorSystem_只标记最后一块(t *testing.T) {
	// 探针 E4 三栏。
	in := []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b", "cache_control": map[string]any{"x": float64(1)}},
	}

	// 支持缓存
	got := reanchorSystemCacheControl(in, true)
	if len(got) != 2 {
		t.Fatalf("应有 2 块, got %d", len(got))
	}
	if _, has := mustJSON(t, got[0])["cache_control"]; has {
		t.Fatalf("第一块不应带 cc, got %v", got[0])
	}
	cc := mustJSON(t, mustJSON(t, got[1])["cache_control"])
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("最后一块应是 {ephemeral,1h}, got %v", cc)
	}
	// ★ 原对象不应被污染: 参考实现是 `map(...)` 出新切片 + `{...rest}` 新对象,
	//   故入参那块仍保留它自己的 cache_control {x:1}。
	origCC := mustJSON(t, mustJSON(t, in[1])["cache_control"])
	if _, has := origCC["x"]; !has {
		t.Fatalf("入参的 cc 应保持原样 {x:1}, got %v", origCC)
	}
	if _, has := origCC["ttl"]; has {
		t.Fatalf("入参的 cc 不应被换成 ephemeral 标记, got %v", origCC)
	}

	// 不支持缓存 -> 全部剔除 cc
	got2 := reanchorSystemCacheControl(in, false)
	for i, b := range got2 {
		if _, has := mustJSON(t, b)["cache_control"]; has {
			t.Fatalf("不支持缓存时第 %d 块不应带 cc, got %v", i, b)
		}
	}

	// 空数组
	if got := reanchorSystemCacheControl([]any{}, true); len(got) != 0 {
		t.Fatalf("空数组应返回空, got %v", got)
	}
}

// --- reanchorToolsCacheControl ---

func TestReanchorTools_跳过defer_loading(t *testing.T) {
	// 探针 E5 五栏。

	// 常规: 标记最后一个
	got := reanchorToolsCacheControl([]any{
		map[string]any{"name": "A"},
		map[string]any{"name": "B"},
	}, true)
	if len(got) != 2 {
		t.Fatalf("应有 2 个 tool, got %d", len(got))
	}
	if _, has := mustJSON(t, got[0])["cache_control"]; has {
		t.Fatalf("A 不应带 cc, got %v", got[0])
	}
	cc := mustJSON(t, mustJSON(t, got[1])["cache_control"])
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("B 应是 {ephemeral,1h}, got %v", cc)
	}

	// 尾部 defer_loading -> 标记前一个
	got2 := reanchorToolsCacheControl([]any{
		map[string]any{"name": "A"},
		map[string]any{"name": "B", "defer_loading": true},
	}, true)
	if _, has := mustJSON(t, got2[0])["cache_control"]; !has {
		t.Fatalf("A 应被标记(尾部是 defer_loading), got %v", got2[0])
	}
	if _, has := mustJSON(t, got2[1])["cache_control"]; has {
		t.Fatalf("defer_loading 的 B 不应被标记, got %v", got2[1])
	}

	// 全部 defer_loading -> 一个都不标记
	got3 := reanchorToolsCacheControl([]any{
		map[string]any{"name": "A", "defer_loading": true},
	}, true)
	if _, has := mustJSON(t, got3[0])["cache_control"]; has {
		t.Fatalf("全 defer_loading 时不应标记, got %v", got3[0])
	}

	// ★ defer_loading=false 是**假值** -> 可被标记(探针 E5 "defer_loading=false")
	got4 := reanchorToolsCacheControl([]any{
		map[string]any{"name": "A", "defer_loading": false},
	}, true)
	if _, has := mustJSON(t, got4[0])["cache_control"]; !has {
		t.Fatalf("defer_loading=false 应可被标记, got %v", got4[0])
	}

	// 不支持缓存 -> 只清不标
	got5 := reanchorToolsCacheControl([]any{
		map[string]any{"name": "A", "cache_control": map[string]any{"old": true}},
	}, false)
	if _, has := mustJSON(t, got5[0])["cache_control"]; has {
		t.Fatalf("不支持缓存时应清掉 cc, got %v", got5[0])
	}
}

// --- markSecondToLastUserCacheControl ---

func TestMarkSecondToLastUser_需要至少两条(t *testing.T) {
	// 探针 E6 三栏。

	txt := func(s string) map[string]any {
		return map[string]any{"type": "text", "text": s}
	}

	// 三个 user -> 标记第二个
	three := []any{
		map[string]any{"role": "user", "content": []any{txt("u1")}},
		map[string]any{"role": "assistant", "content": []any{txt("a1")}},
		map[string]any{"role": "user", "content": []any{txt("u2")}},
		map[string]any{"role": "assistant", "content": []any{txt("a2")}},
		map[string]any{"role": "user", "content": []any{txt("u3")}},
	}
	if !markSecondToLastUserCacheControl(three) {
		t.Fatalf("三个 user 应标记成功")
	}
	// u1 无 cc
	if _, has := mustJSON(t, three[0].(map[string]any)["content"].([]any)[0])["cache_control"]; has {
		t.Fatalf("u1 不应被标记, got %v", three[0])
	}
	// u2 有 cc
	if _, has := mustJSON(t, three[2].(map[string]any)["content"].([]any)[0])["cache_control"]; !has {
		t.Fatalf("u2(倒数第二)应被标记, got %v", three[2])
	}
	// u3 无 cc
	if _, has := mustJSON(t, three[4].(map[string]any)["content"].([]any)[0])["cache_control"]; has {
		t.Fatalf("u3 不应被标记, got %v", three[4])
	}

	// 两个 user -> 标记第一个(即倒数第二)
	two := []any{
		map[string]any{"role": "user", "content": []any{txt("u1")}},
		map[string]any{"role": "assistant", "content": []any{txt("a1")}},
		map[string]any{"role": "user", "content": []any{txt("u2")}},
	}
	if !markSecondToLastUserCacheControl(two) {
		t.Fatalf("两个 user 应标记成功")
	}
	if _, has := mustJSON(t, two[0].(map[string]any)["content"].([]any)[0])["cache_control"]; !has {
		t.Fatalf("u1(倒数第二)应被标记, got %v", two[0])
	}
	if _, has := mustJSON(t, two[2].(map[string]any)["content"].([]any)[0])["cache_control"]; has {
		t.Fatalf("u2 不应被标记, got %v", two[2])
	}

	// 一个 user -> 不标记
	one := []any{map[string]any{"role": "user", "content": []any{txt("u1")}}}
	if markSecondToLastUserCacheControl(one) {
		t.Fatalf("一个 user 不应标记")
	}
}

// --- markLastAssistantCacheControl ---

func TestMarkLastAssistant_只标记最后一条有内容的(t *testing.T) {
	txt := func(s string) map[string]any {
		return map[string]any{"type": "text", "text": s}
	}
	msgs := []any{
		map[string]any{"role": "user", "content": []any{txt("u1")}},
		map[string]any{"role": "assistant", "content": []any{txt("a1")}},
		map[string]any{"role": "user", "content": []any{txt("u2")}},
		map[string]any{"role": "assistant", "content": []any{txt("a2")}},
	}
	if !markLastAssistantCacheControl(msgs) {
		t.Fatalf("应标记成功")
	}
	if _, has := mustJSON(t, msgs[1].(map[string]any)["content"].([]any)[0])["cache_control"]; has {
		t.Fatalf("较早的 assistant 不应被标记(只标一条), got %v", msgs[1])
	}
	if _, has := mustJSON(t, msgs[3].(map[string]any)["content"].([]any)[0])["cache_control"]; !has {
		t.Fatalf("最后一条 assistant 应被标记, got %v", msgs[3])
	}
}

func TestMarkLastAssistant_跳过空content(t *testing.T) {
	// 尾部 assistant content 为空 -> 往前找有内容的
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "a1"}}},
		map[string]any{"role": "assistant", "content": []any{}},
	}
	if !markLastAssistantCacheControl(msgs) {
		t.Fatalf("应跳过空 content 标记前一条")
	}
	if _, has := mustJSON(t, msgs[0].(map[string]any)["content"].([]any)[0])["cache_control"]; !has {
		t.Fatalf("前一条 assistant 应被标记, got %v", msgs[0])
	}
}

func TestMarkLastAssistant_无assistant返回false(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u"}}},
	}
	if markLastAssistantCacheControl(msgs) {
		t.Fatalf("无 assistant 应返回 false")
	}
}

// --- supportsPromptCachingFor ---

func TestSupportsPromptCachingFor_白名单(t *testing.T) {
	// 照抄 claudeHelper.ts:366-367。
	yes := []string{"claude", "anthropic-compatible-cc-xxx", "anthropic-compatible-"}
	for _, p := range yes {
		if !supportsPromptCachingFor(p) {
			t.Fatalf("%q 应支持 prompt caching", p)
		}
	}
	no := []string{"", "openai", "kimi-coding", "anthropic", "claude-3", "zai", "glmt"}
	for _, p := range no {
		if supportsPromptCachingFor(p) {
			t.Fatalf("%q 不应支持 prompt caching", p)
		}
	}
}

// --- stripMessageCacheControl ---

func TestStripMessageCacheControl_就地删除(t *testing.T) {
	m := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "a", "cache_control": map[string]any{"x": true}},
		map[string]any{"type": "text", "text": "b"},
	}}
	stripMessageCacheControl(m)
	blocks := m["content"].([]any)
	for i, b := range blocks {
		if _, has := mustJSON(t, b)["cache_control"]; has {
			t.Fatalf("第 %d 块仍带 cc, got %v", i, b)
		}
	}
}

func TestStripMessageCacheControl_content非数组noop(t *testing.T) {
	m := map[string]any{"role": "user", "content": "plain"}
	stripMessageCacheControl(m)
	if m["content"] != "plain" {
		t.Fatalf("content 非数组时应 no-op, got %v", m["content"])
	}
}

// --- CLAUDE_FORMAT_PROVIDERS_WITHOUT_OUTPUT_CONFIG ---

func TestClaudeFormatProvidersWithoutOutputConfig(t *testing.T) {
	// 照抄 claudeHelper.ts:16 的 Set(["minimax", "minimax-cn"])。
	if !claudeFormatProvidersWithoutOutputConfig["minimax"] {
		t.Fatalf("minimax 应在剔除清单里")
	}
	if !claudeFormatProvidersWithoutOutputConfig["minimax-cn"] {
		t.Fatalf("minimax-cn 应在剔除清单里")
	}
	if len(claudeFormatProvidersWithoutOutputConfig) != 2 {
		t.Fatalf("清单应恰好 2 项, got %d", len(claudeFormatProvidersWithoutOutputConfig))
	}
	if claudeFormatProvidersWithoutOutputConfig["claude"] {
		t.Fatalf("claude 不应在清单里")
	}
}
