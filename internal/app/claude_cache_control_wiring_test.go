package app

import (
	"os"
	"strings"
	"testing"
)

// reanchorClaudePromptCache 的接入级测试。
//
// ★ 权威值来自 .negbak/probe_cachecontrol.mjs 的 E3–E6 各栏。
// ★ 逐字段断言, 不比对 JSON 文本(Go/Node 键序不同)。

// ccBody 构造一个**客户端不带任何 marker** 的 Claude 形态 body ——
// 这正是 Codex / Cline 的真实形态, 也是重锚逻辑真正生效的场景。
func ccBody() map[string]any {
	return map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "sys1"},
			map[string]any{"type": "text", "text": "sys2"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "u1"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "a1"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "u2"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "T1"},
			map[string]any{"name": "T2"},
		},
	}
}

// ccBodyWithClientMarkers 构造一个客户端**自带** marker 的 body
// (模拟 Claude Code 这类自己管 markdown 的客户端)。
func ccBodyWithClientMarkers() map[string]any {
	return map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "sys1"},
			map[string]any{"type": "text", "text": "sys2", "cache_control": map[string]any{"stale": true}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "u1", "cache_control": map[string]any{"stale": true}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "a1"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "u2"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "T1", "cache_control": map[string]any{"stale": true}},
			map[string]any{"name": "T2"},
		},
	}
}

// --- claude (支持 prompt caching) ---

func TestReanchorCache_claude全链路(t *testing.T) {
	params := ccBody()
	reanchorClaudePromptCache(params, "claude")

	// system: 只最后一块带 {ephemeral,1h}, 旧的 stale 被剔除
	sys := params["system"].([]any)
	if _, has := mustJSON(t, sys[0])["cache_control"]; has {
		t.Fatalf("sys1 不应带 cc, got %v", sys[0])
	}
	cc := mustJSON(t, mustJSON(t, sys[1])["cache_control"])
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("sys2 应是 {ephemeral,1h}, got %v", cc)
	}
	if _, has := cc["stale"]; has {
		t.Fatalf("sys2 的旧 cc 应被替换, got %v", cc)
	}

	// messages: 第三轮的 u1 是倒数第二条 user -> 被标记
	msgs := params["messages"].([]any)
	u1 := mustJSON(t, msgs[0].(map[string]any)["content"].([]any)[0])
	if u1b := mustJSON(t, u1["cache_control"]); u1b["type"] != "ephemeral" {
		t.Fatalf("u1(倒数第二条 user) 应被标记, got %v", u1)
	}
	// a1 是最后一条 assistant -> 被标记
	a1 := mustJSON(t, msgs[1].(map[string]any)["content"].([]any)[0])
	if a1b := mustJSON(t, a1["cache_control"]); a1b["type"] != "ephemeral" {
		t.Fatalf("a1(最后一条 assistant) 应被标记, got %v", a1)
	}
	// u2 是最后一条 user -> 不被标记(只有倒数第二条才标)
	u2 := mustJSON(t, msgs[2].(map[string]any)["content"].([]any)[0])
	if _, has := u2["cache_control"]; has {
		t.Fatalf("u2(最后一条 user) 不应被标记, got %v", u2)
	}

	// tools: 只最后一条带 {ephemeral,1h}
	tools := params["tools"].([]any)
	if _, has := mustJSON(t, tools[0])["cache_control"]; has {
		t.Fatalf("T1 不应带 cc, got %v", tools[0])
	}
	tc := mustJSON(t, mustJSON(t, tools[1])["cache_control"])
	if tc["type"] != "ephemeral" || tc["ttl"] != "1h" {
		t.Fatalf("T2 应是 {ephemeral,1h}, got %v", tc)
	}
}

func TestReanchorCache_已有marker则整体passthrough(t *testing.T) {
	// 客户端自带 marker -> 一个都不动(参考实现 preserveCacheControl=true 那一路)。
	params := ccBodyWithClientMarkers()
	// 确认 bodyHasAnyCacheControl 为真(靠 stale marker)
	if !bodyHasAnyCacheControl(params) {
		t.Fatalf("前置: 该 body 应被判定为带 marker")
	}
	reanchorClaudePromptCache(params, "claude")

	// system: 客户端的原样保留(不换成 ephemeral/1h, 也不剔 stale)
	sys := params["system"].([]any)
	if _, has := mustJSON(t, sys[0])["cache_control"]; has {
		t.Fatalf("sys1 本来就没有 marker, 不应凭空新增, got %v", sys[0])
	}
	cc := mustJSON(t, mustJSON(t, sys[1])["cache_control"])
	if cc["stale"] != true {
		t.Fatalf("sys2 应保留客户端 marker {stale:true}, got %v", cc)
	}
	if _, has := cc["ttl"]; has {
		t.Fatalf("sys2 不应被换成 ephemeral 标记, got %v", cc)
	}

	// messages: 客户端的 u1 marker 原样保留
	u1 := mustJSON(t, mustJSON(t, params["messages"].([]any)[0].(map[string]any)["content"].([]any)[0])["cache_control"])
	if u1["stale"] != true {
		t.Fatalf("u1 应保留客户端 marker, got %v", u1)
	}

	// tools: 客户端的 T1 marker 原样保留
	t1 := mustJSON(t, mustJSON(t, params["tools"].([]any)[0])["cache_control"])
	if t1["stale"] != true {
		t.Fatalf("T1 应保留客户端 marker, got %v", t1)
	}
}

// --- 非 claude (不支持 prompt caching) ---

func TestReanchorCache_非claude只清不标(t *testing.T) {
	// supportsPromptCaching=false 时: 标记函数一个都不调, 但**清理**照常。
	//
	// ★ 用干净 body(无客户端 marker)走非 passthrough 路径 —— 这样 system/tools
	//   的"清"与"不标"都能观察到。清理自身的可观察后果在
	//   TestStripMessageCacheControl_就地删除 / TestReanchorSystem 里已单独锁定。
	params := ccBody()
	reanchorClaudePromptCache(params, "kimi-coding")

	// 干净 body + 不支持缓存 -> system/tools 一个 marker 都不应新增
	for i, raw := range params["system"].([]any) {
		if _, has := mustJSON(t, raw)["cache_control"]; has {
			t.Fatalf("system 第 %d 块不应带 cc, got %v", i, raw)
		}
	}
	for i, raw := range params["tools"].([]any) {
		if _, has := mustJSON(t, raw)["cache_control"]; has {
			t.Fatalf("tools 第 %d 个不应带 cc, got %v", i, raw)
		}
	}
	for i, raw := range params["messages"].([]any) {
		msg := raw.(map[string]any)
		content, _ := msg["content"].([]any)
		for j, b := range content {
			if _, has := mustJSON(t, b)["cache_control"]; has {
				t.Fatalf("messages[%d].content[%d] 不应带 cc, got %v", i, j, b)
			}
		}
	}
}

func TestReanchorCache_非claude清理已有marker(t *testing.T) {
	// 清理路径的可观察后果: 用**自带 marker** 的 body, 但绕过 passthrough
	// 判定 —— 直接调各清理函数(它们在支持缓存时也会被调用)。
	params := ccBodyWithClientMarkers()

	// system 重锚(不支持缓存) -> 全清
	sys := reanchorSystemCacheControl(params["system"].([]any), false)
	for i, raw := range sys {
		if _, has := mustJSON(t, raw)["cache_control"]; has {
			t.Fatalf("system 第 %d 块不应带 cc, got %v", i, raw)
		}
	}
	// tools 重锚(不支持缓存) -> 全清
	tools := reanchorToolsCacheControl(params["tools"].([]any), false)
	for i, raw := range tools {
		if _, has := mustJSON(t, raw)["cache_control"]; has {
			t.Fatalf("tools 第 %d 个不应带 cc, got %v", i, raw)
		}
	}
	// messages 清理 -> 全清
	for _, raw := range params["messages"].([]any) {
		stripMessageCacheControl(raw.(map[string]any))
	}
	for i, raw := range params["messages"].([]any) {
		content, _ := raw.(map[string]any)["content"].([]any)
		for j, b := range content {
			if _, has := mustJSON(t, b)["cache_control"]; has {
				t.Fatalf("messages[%d].content[%d] 仍带 cc, got %v", i, j, b)
			}
		}
	}
}

// --- output_config 剥离 ---

func TestReanchorCache_minimax剥离output_config(t *testing.T) {
	// 照抄 claudeHelper.ts:16 + :342-347。
	for _, p := range []string{"minimax", "minimax-cn"} {
		params := map[string]any{
			"output_config": map[string]any{"effort": "high"},
			"messages":      []any{},
		}
		reanchorClaudePromptCache(params, p)
		if _, has := params["output_config"]; has {
			t.Fatalf("%s 的 output_config 应被剥离, got %v", p, params["output_config"])
		}
	}
	// 其它 provider 不动该字段
	params := map[string]any{
		"output_config": map[string]any{"effort": "high"},
		"messages":      []any{},
	}
	reanchorClaudePromptCache(params, "claude")
	if _, has := params["output_config"]; !has {
		t.Fatalf("非 minimax 系不应剥离 output_config")
	}
}

// --- 空与边界 ---

func TestReanchorCache_空body不panic(t *testing.T) {
	params := map[string]any{}
	reanchorClaudePromptCache(params, "claude")
	if len(params) != 0 {
		t.Fatalf("空 body 不应被写入任何键, got %v", params)
	}
}

func TestReanchorCache_system是字符串不panic(t *testing.T) {
	// 探针 E3 "system 是字符串" -> Array.isArray 为假 -> 跳过。
	params := map[string]any{"system": "raw system string"}
	reanchorClaudePromptCache(params, "claude")
	if params["system"] != "raw system string" {
		t.Fatalf("字符串 system 应原样保留, got %v", params["system"])
	}
}

// TestReanchorCache接入_源码层锁死 用源码文本断言锁住接线位置与作用域。
//
// 与 TestChatWithKey接线_源码层锁死 同样的理由: 上面的测试是**复刻**调用,
// 证明不了真实接线在。本用例直接检查 providers_chat.go。
func TestReanchorCache接入_源码层锁死(t *testing.T) {
	src, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读 providers_chat.go 失败: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, "reanchorClaudePromptCache(params, normalizeProviderID(p.name))") {
		t.Fatalf("接线缺失: reanchorClaudePromptCache 未在 providers_chat.go 中被调用")
	}

	// ★ 作用域必须与 tool 链**同一个 anthropic 分支** —— 两者在参考实现里
	//   都由 `targetFormat === claude` 这一条件统辖。
	//
	// 判据: anthropic 分支守卫之后、非 anthropic 分支守卫之前, **恰好一次**调用。
	//   ★ 只判"第一次出现的位置"是不够的 —— 把调用同时放进两个分支也能骗过
	//     单点 Index 判定(负向验证 NEG-CACHE-A 实测)。这里改为:
	//       ① 调用次数 == 1;
	//       ② 该唯一调用点落在 (anthropicIf, elseIf) 开区间内。
	anthropicIf := strings.Index(s, `if cfg.APIType == "anthropic" {`)
	callMarker := "reanchorClaudePromptCache(params, normalizeProviderID(p.name))"
	elseIf := strings.Index(s, `} else if msgs, ok := params["messages"]; ok {`)

	if anthropicIf < 0 {
		t.Fatalf("结构缺失: 未找到 anthropic 分支守卫")
	}
	if elseIf < 0 {
		t.Fatalf("结构缺失: 未找到非 anthropic 分支守卫")
	}
	if n := strings.Count(s, callMarker); n != 1 {
		t.Fatalf("接线次数错误: reanchorClaudePromptCache 应恰好调用 1 次, got %d", n)
	}
	call := strings.Index(s, callMarker)
	if !(anthropicIf < call && call < elseIf) {
		t.Fatalf("接线作用域错误: 调用点(%d)必须落在 anthropic 分支(%d)与非 anthropic 分支(%d)之间",
			call, anthropicIf, elseIf)
	}

	// ★ output_config 剥离必须在 reanchorClaudePromptCache **内部最前**,
	//   不能散在别处 —— reference: claudeHelper.ts:342-347 是函数体第一段。
	if !strings.Contains(s, "reanchorClaudePromptCache") {
		t.Fatalf("接线缺失")
	}
	srcCC, err := os.ReadFile("claude_cache_control.go")
	if err != nil {
		t.Fatalf("读 claude_cache_control.go 失败: %v", err)
	}
	cc := string(srcCC)
	delIdx := strings.Index(cc, `delete(params, "output_config")`)
	preserveIdx := strings.Index(cc, `preserveCacheControl := bodyHasAnyCacheControl(params)`)
	if delIdx < 0 || preserveIdx < 0 {
		t.Fatalf("结构缺失: output_config 剥离或 preserve 判定未找到")
	}
	if !(delIdx < preserveIdx) {
		t.Fatalf("顺序错误: output_config 剥离(%d)必须早于 preserve 判定(%d)", delIdx, preserveIdx)
	}

	// 两个标记函数的调用顺序: user 在前, assistant 在后(照抄 claudeHelper.ts:503 → :530)
	userMark := strings.Index(cc, "markSecondToLastUserCacheControl(messages)")
	asstMark := strings.Index(cc, "markLastAssistantCacheControl(messages)")
	if userMark < 0 || asstMark < 0 {
		t.Fatalf("结构缺失: 两个标记函数未找到")
	}
	if !(userMark < asstMark) {
		t.Fatalf("顺序错误: user 标记(%d)必须在 assistant 标记(%d)之前", userMark, asstMark)
	}
}
