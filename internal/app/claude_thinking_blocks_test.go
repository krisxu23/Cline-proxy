package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 claude_thinking_blocks.go 的权威值。
//
// ★ 期望值全部来自 Node 实跑参考实现（见 .negbak/probe_thinkingblocks.mjs
//   的 T1–T15 各栏），不是读代码推断的。
//
// ★ 探针修正记录（重要）: 初版探针把 `modelTargetsClaude` 写死为 true，
//   导致 glmt（非 Anthropic 上游，targetsClaude 实际为 false）也走
//   redacted_thinking 分支 —— 那正是参考实现要**避免**的错误形态
//   （非 Anthropic 上游无法校验签名 blob，会 400 "Invalid signature"）。
//   修正后 modelTargetsClaude 作为入参传入，T11–T13 才暴露出 plain thinking 分支。

// tbSig 是 DEFAULT_THINKING_CLAUDE_SIGNATURE 的缩写别名，仅供本文件断言用。
const tbSig = "EpwGCkYIChgCKkCzVUuRrg7CcglSUWEef4rH6o35g9UYS8ZPe0/VomQTBsFx6sttYNj5l8GqgW6ejuHyYqpFToxIbZl0bw17l5dJEgzCnqDO0Z8fRlMrNgsaDLS1cnCjC53KBqE0CCIwAADQdo1eO+7qPAmo8J4WR3JPmr92S97kmvr5K1iPMiOpkZNj8mEXW8uzBoOJs/9ZKoMFiqHJ3UObwaJDqFOW70E9oCwDoc6jesaWVAEdN5vWfKMpIkjFJjECdjIdkxyJNJ8Ib8yXVal3qwE7uThoPRqSZDdHB5mmwPEjWE/90cSYCbtX2YsJki1265CabBb8/QEkODXg4kgRrL+c8e8rRXz/dr1RswvaPuzEdGKHRNi9UooNUeOK4/ebx1KkP9YZttyohN9GWqlts36kOoW0Cfie/ABDgF9g534BPth/sstxDM6d79QlRmh6NxizyTF74DXJI34u0M4tTRchqE5pAq85SgdJaa+dix1yJPMji8m6nZkwJbscJb9rdc2MKyKWjz8QL2+rTSSuZ2F1k1qSsW0xNcI7qLcI12Vncfn/VqY6YOIZy/saZBR0ezXvN6g+UYbuIdyVg7AyIFZt3nbrO7/kmOEb2VKzygwklHGEIJHfFgMpH3JSrAzbZIowVHOF7VaJ+KXRFDCFin7hHTOiOsdg+1ij1mML9Z/x/9CP4b7OUcaQm1llDZPSHc6rZMNL3DdB+fW5YfmNgKU35S+7AMtA10nVILzDAk1UV4T2K9Do09JlI6rjOs9UuULlIN2Z0eE8YTlANR6uQcw7lMcdfqYE8tke4rDKc2dDiaS5vVe45VewICNpdXGN11yw8QqH7p27CR1HtN30e0tHXOR3bIwWk/Yb6O5fTaKG6Ri8e5ZCPvdD9HqepVi188nM0iTjJqL58F3ni04ECIhcbyaQWnuTes1Kw4CMwiZDLQkk8Hgz7HkUOf1btQTF/0nhD7ry0n0hAEg2PaDM3V6TjOjf4hEldRmeqERcQF1PfgKb6ZM12rlIIfUqKACczWJSzTV158+47HX36o0cgux6nFlv/DE+sEiRVxgB"

// tbMsgs 把 JSON 字面量解成 messages（探针输入的直接搬运）。
func tbMsgs(t *testing.T, s string) []any {
	t.Helper()
	var a []any
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatalf("解析 messages 失败: %v", err)
	}
	return a
}

// tbJSON 取某条消息 content 的规范化 JSON（键序无关）。
func tbJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// tbRun 跑一次 applyClaudeThinkingBlocks 并返回 (changed, messages)。
func tbRun(
	t *testing.T,
	msgsJSON string,
	body map[string]any,
	provider string,
	targetsClaude bool,
	lookup map[string]string,
) (bool, []any, int) {
	t.Helper()
	msgs := tbMsgs(t, msgsJSON)
	replays := 0
	var lf func(string) (string, bool)
	if lookup != nil {
		lf = func(id string) (string, bool) {
			v, ok := lookup[id]
			return v, ok
		}
	}
	changed := applyClaudeThinkingBlocks(msgs, body, provider, targetsClaude, lf, func() { replays++ })
	return changed, msgs, replays
}

// tbContent 取第 i 条消息的 content。
func tbContent(t *testing.T, msgs []any, i int) []any {
	t.Helper()
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("msgs[%d] 不是对象", i)
	}
	c, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("msgs[%d].content 不是数组", i)
	}
	return c
}

// tbBlock 取 content 里的第 j 个块。
func tbBlock(t *testing.T, content []any, j int) map[string]any {
	t.Helper()
	b, ok := content[j].(map[string]any)
	if !ok {
		t.Fatalf("content[%d] 不是对象", j)
	}
	return b
}

// ─────────────────────────── T1 ───────────────────────────

func TestThinking_T1_原生claude前置redacted(t *testing.T) {
	// 探针 T1: claude + thinking开 + 最后是 user + assistant 有 tool_use 无 thinking
	//           -> unshift redacted_thinking{data: <默认签名>}，零 replay。
	changed, msgs, replays := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	if replays != 0 {
		t.Fatalf("不应有 replay, got %d", replays)
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("assistant content 应有 2 块, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "redacted_thinking" {
		t.Fatalf("首块应是 redacted_thinking, got %v", b0["type"])
	}
	if b0["data"] != tbSig {
		t.Fatalf("data 应是默认签名")
	}
	if b1 := tbBlock(t, c, 1); b1["type"] != "tool_use" {
		t.Fatalf("次块应仍是 tool_use, got %v", b1["type"])
	}
}

// ─────────────────────────── T2 / T3 / T4 ───────────────────────────

func TestThinking_T2_无tool_use不动(t *testing.T) {
	// 探针 T2: 有 thinking 但 assistant 无 tool_use -> hasToolUse=false -> 不注入。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if changed {
		t.Fatalf("不应发生改动")
	}
	if got := tbJSON(t, msgs); got != `[{"content":[{"text":"hi","type":"text"}],"role":"user"},{"content":[{"text":"hello","type":"text"}],"role":"assistant"},{"content":[{"text":"more","type":"text"}],"role":"user"}]` {
		t.Fatalf("messages 应原样, got %s", got)
	}
}

func TestThinking_T3_thinking关不注入(t *testing.T) {
	// 探针 T3: body 无 thinking -> thinkingEnabled=false -> 不注入。
	changed, _, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{}, "claude", true, nil)

	if changed {
		t.Fatalf("thinking 未开启时不应改动")
	}
}

func TestThinking_T4_最后是assistant不注入(t *testing.T) {
	// 探针 T4: thinking 开但最后一条是 assistant -> lastMessageIsUser=false -> 不注入。
	changed, _, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if changed {
		t.Fatalf("最后一条非 user 时不应改动")
	}
}

// ─────────────────────────── T5 / T6 / T7 ───────────────────────────

func TestThinking_T5_旧回合真签名thinking改写成redacted(t *testing.T) {
	// 探针 T5: 旧 assistant 回合的 thinking{signature:"REAL_SIG"} 被改写成
	//          redacted_thinking{data:<默认签名>}；最新回合是 text 无 tool_use。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"real reasoning","signature":"REAL_SIG"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
		{"role":"assistant","content":[{"type":"text","text":"done"}]},
		{"role":"user","content":[{"type":"text","text":"again"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "redacted_thinking" {
		t.Fatalf("旧回合 thinking 应改写成 redacted_thinking, got %v", b0["type"])
	}
	if b0["data"] != tbSig {
		t.Fatalf("data 应是默认签名")
	}
	// ★ thinking / signature 两个键都必须被删掉（参考实现 delete 语义）。
	if _, has := b0["thinking"]; has {
		t.Fatalf("thinking 键应被删除")
	}
	if _, has := b0["signature"]; has {
		t.Fatalf("signature 键应被删除")
	}
}

func TestThinking_T6_最新回合真签名原样透传(t *testing.T) {
	// 探针 T6: 最新 assistant 回合带真签名 thinking -> `continue` 整段跳过，
	//          **连字段清理都不做**（签名/文本逐字保留）。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"real reasoning","signature":"REAL_SIG"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if changed {
		t.Fatalf("最新回合真签名不应有改动")
	}
	c := tbContent(t, msgs, 1)
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("type 应逐字保留为 thinking, got %v", b0["type"])
	}
	if b0["thinking"] != "real reasoning" {
		t.Fatalf("thinking 文本应逐字保留, got %v", b0["thinking"])
	}
	if b0["signature"] != "REAL_SIG" {
		t.Fatalf("signature 应逐字保留, got %v", b0["signature"])
	}
}

func TestThinking_T7_最新回合空签名走净化(t *testing.T) {
	// 探针 T7: 最新 assistant 回合的 thinking{signature:""} —— #6953 的关键回归。
	//          空签名不是真签名，透传给 Anthropic 必 400，故必须走净化路径
	//          （与旧回合同样改写成 redacted_thinking + 默认签名）。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"synthetic","signature":""},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "claude", true, nil)

	if !changed {
		t.Fatalf("空签名必须走净化路径，应发生改动")
	}
	c := tbContent(t, msgs, 1)
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "redacted_thinking" {
		t.Fatalf("空签名应改写成 redacted_thinking, got %v", b0["type"])
	}
	if b0["data"] != tbSig {
		t.Fatalf("data 应是默认签名")
	}
}

// ─────────────────────────── T8 / T9 / T10 (kimi-coding) ───────────────────────────

func TestThinking_T8_kimi前置空thinking(t *testing.T) {
	// 探针 T8: kimi-coding + thinking 开 + tool_use -> unshift thinking{thinking:""}。
	//
	// ★ kimi 的形态特点: 前置块的 thinking 是**空串**（不是占位符）——
	//   参考实现 :699-703 只给 `thinking: ""`，因为 kimi-coding 把"空文本"
	//   当作"缺失"处理，而占位符反而是它认识的普通文本。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "kimi-coding", true, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("应有 2 块, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("首块应是 thinking, got %v", b0["type"])
	}
	if b0["thinking"] != "" {
		t.Fatalf("kimi 前置块 thinking 应是空串, got %q", b0["thinking"])
	}
	if _, has := b0["data"]; has {
		t.Fatalf("kimi 不应带 data")
	}
}

func TestThinking_T9_kimi剥signature留文本(t *testing.T) {
	// 探针 T9: kimi-coding + 既有 thinking{thinking, signature} ->
	//          保留文本、删 data 与 signature。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"some reasoning","signature":"SIG"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "kimi-coding", true, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("应仍是 2 块（不新增）, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("type 应是 thinking, got %v", b0["type"])
	}
	if b0["thinking"] != "some reasoning" {
		t.Fatalf("文本应保留, got %v", b0["thinking"])
	}
	if _, has := b0["signature"]; has {
		t.Fatalf("signature 应被删除")
	}
	if _, has := b0["data"]; has {
		t.Fatalf("data 应被删除")
	}
}

func TestThinking_T10_kimi_redacted转thinking(t *testing.T) {
	// 探针 T10: kimi-coding + redacted_thinking{data:"BLOB"} ->
	//          type 改 thinking、thinking 补空串（原块无 thinking 字段）、删 data。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"BLOB"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "kimi-coding", true, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("应仍是 2 块, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("redacted_thinking 应转成 thinking, got %v", b0["type"])
	}
	if b0["thinking"] != "" {
		t.Fatalf("原块无 thinking 字段应补空串, got %q", b0["thinking"])
	}
	if _, has := b0["data"]; has {
		t.Fatalf("data 应被删除")
	}
}

// ─────────────────────────── T11 / T12 / T13 (glmt, targetsClaude=false) ───────────────────────────

func TestThinking_T11_非Anthropic上游前置占位符(t *testing.T) {
	// 探针 T11: glmt + targetsClaude=false + cache 未命中
	//           -> unshift thinking{thinking: 占位符}（**不是** redacted_thinking）。
	//
	// ★ 这条是"写死 modelTargetsClaude=true"会踩坑的证据: 写死会产出
	//   redacted_thinking{data:<签名>}，而 glmt 一类非 Anthropic 上游无法校验
	//   该签名 blob，正是参考实现注释里点名的 400 "Invalid signature in
	//   thinking block"。必须按 provider 能力选形状。
	changed, msgs, replays := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "glmt", false, map[string]string{})

	if !changed {
		t.Fatalf("应发生改动")
	}
	if replays != 0 {
		t.Fatalf("cache 未命中不应 replay, got %d", replays)
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("应有 2 块, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("非 Anthropic 上游应是 plain thinking, got %v", b0["type"])
	}
	if b0["thinking"] != nonAnthropicThinkingPlaceholder {
		t.Fatalf("应是占位符, got %v", b0["thinking"])
	}
	if _, has := b0["data"]; has {
		t.Fatalf("plain thinking 不应带 data")
	}
}

func TestThinking_T12_非Anthropic上游cache命中用真实文本(t *testing.T) {
	// 探针 T12: glmt + cache 命中 t1 -> 前置 thinking 用真实推理文本，replay +1。
	changed, msgs, replays := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "glmt", false,
		map[string]string{"t1": "cached reasoning text"})

	if !changed {
		t.Fatalf("应发生改动")
	}
	if replays != 1 {
		t.Fatalf("命中缓存应 replay 1 次, got %d", replays)
	}
	c := tbContent(t, msgs, 1)
	b0 := tbBlock(t, c, 0)
	if b0["thinking"] != "cached reasoning text" {
		t.Fatalf("应用缓存的真实文本, got %v", b0["thinking"])
	}
}

func TestThinking_T13_既有空thinking块填占位符(t *testing.T) {
	// 探针 T13: glmt + 既有 thinking{thinking:""} + cache 未命中 -> 填占位符。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "glmt", false, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	if len(c) != 2 {
		t.Fatalf("应仍是 2 块（复用既有的）, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "thinking" {
		t.Fatalf("type 应是 thinking, got %v", b0["type"])
	}
	if b0["thinking"] != nonAnthropicThinkingPlaceholder {
		t.Fatalf("空文本应填占位符, got %v", b0["thinking"])
	}
}

// ─────────────────────────── T14 / T15 ───────────────────────────

func TestThinking_T14_字符串content不误判(t *testing.T) {
	// 探针 T14: assistant content 是字符串 -> ensureMessageContentArray 包成
	//           [{type:text,text:...}]，无 tool_use 故不注入；changed=false。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"just text"},
		{"role":"user","content":"more"}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}}, "glmt", false, nil)

	if changed {
		t.Fatalf("纯文本回合不应有改动")
	}
	// ensureMessageContentArray 是**就地改写**（参考实现 :286-293）——
	// 字符串被换成数组，且新数组第一个元素是 {type:"text", text:<原串>}。
	c := tbContent(t, msgs, 1)
	if len(c) != 1 {
		t.Fatalf("应包成 1 块, got %d", len(c))
	}
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "text" || b0["text"] != "just text" {
		t.Fatalf("应是 {type:text,text:'just text'}, got %v", b0)
	}
}

func TestThinking_T15_anthropicCompatible前缀走redacted(t *testing.T) {
	// 探针 T15: `anthropic-compatible-custom` -> supportsPromptCaching=true
	//           -> supportsRedactedThinking=true（即使 targetsClaude=false）
	//           -> unshift redacted_thinking。
	changed, msgs, _ := tbRun(t, `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"more"}]}
	]`, map[string]any{"thinking": map[string]any{"type": "enabled"}},
		"anthropic-compatible-custom", false, nil)

	if !changed {
		t.Fatalf("应发生改动")
	}
	c := tbContent(t, msgs, 1)
	b0 := tbBlock(t, c, 0)
	if b0["type"] != "redacted_thinking" {
		t.Fatalf("anthropic-compatible-* 应是 redacted_thinking, got %v", b0["type"])
	}
	if b0["data"] != tbSig {
		t.Fatalf("data 应是默认签名")
	}
}

// ─────────────────────────── 辅助判定 ───────────────────────────

func TestThinking_supportsPromptCaching前缀判定(t *testing.T) {
	// 照抄 claudeHelper.ts:366-367。
	yes := []string{"claude", "anthropic-compatible-", "anthropic-compatible-custom", "anthropic-compatible-x"}
	for _, p := range yes {
		if !supportsPromptCachingForProvider(p) {
			t.Fatalf("%q 应命中 supportsPromptCaching", p)
		}
	}
	no := []string{"", "glmt", "kimi-coding", "anthropic", "claudex", "zai"}
	for _, p := range no {
		if supportsPromptCachingForProvider(p) {
			t.Fatalf("%q 不应命中 supportsPromptCaching", p)
		}
	}
}

func TestThinking_isKimiCoding判定(t *testing.T) {
	// 照抄 claudeHelper.ts:368。
	yes := []string{"kimi-coding", "kimi-coding-apikey"}
	for _, p := range yes {
		if !isKimiCodingProvider(p) {
			t.Fatalf("%q 应命中 isKimiCoding", p)
		}
	}
	no := []string{"", "kimi", "kimi-coding-other", "KIMI-CODING"}
	for _, p := range no {
		if isKimiCodingProvider(p) {
			t.Fatalf("%q 不应命中 isKimiCoding", p)
		}
	}
}

func TestThinking_defaultClaudeSignature长度(t *testing.T) {
	// 参考实现的常量长度（避免搬运时被编辑器折行/截断）。
	if len(defaultThinkingClaudeSignature) != 1068 {
		t.Fatalf("默认签名长度应是 1068, got %d", len(defaultThinkingClaudeSignature))
	}
}

func TestThinking_非对象消息跳过不panic(t *testing.T) {
	// 参考实现在 `msg.role` 上直接取值，非对象会在 JS 里报错；Go 侧必须安全跳过。
	msgs := []any{nil, "str", float64(1), map[string]any{"role": "assistant", "content": []any{}}}
	body := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	// 不应 panic。
	_ = applyClaudeThinkingBlocks(msgs, body, "claude", true, nil, nil)
}
