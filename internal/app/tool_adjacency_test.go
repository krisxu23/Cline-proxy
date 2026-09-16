package app

import (
	"os"
	"strings"
	"testing"
)

// 本文件全部期望值由 Node 实跑 D:\OmniRoute\...\contextManager.ts:829-1000 得到,
// 不是推测值。探针脚本见工作区 .negbak/probe_adj.mjs。

func adjJSON(t *testing.T, v any) string {
	t.Helper()
	return normJSON(t, v)
}

// --- fixToolAdjacency ---

func TestFixToolAdjacency_结果不在紧邻下一条则删tool_use(t *testing.T) {
	// Node 实跑: [user("text in between"), user([tool_result t1])]
	// 即 assistant 整条被删(它只剩那个 tool_use), 而更远处的 tool_result 对本函数
	// 不可见 —— 这**不是** bug: 本函数只判"紧邻", 更远处的结果由 fixToolPairs 负责。
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "R"},
		}},
		map[string]any{"role": "user", "content": "text in between"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
	}
	out := fixToolAdjacency(in)
	if len(out) != 2 {
		t.Fatalf("应剩 2 条(assistant 因修空被丢), got %d: %s", len(out), adjJSON(t, out))
	}
	if mustJSON(t, out[0])["content"] != "text in between" {
		t.Fatalf("首条应为中间的 user 文本, got %s", adjJSON(t, out[0]))
	}
}

func TestFixToolAdjacency_结果紧邻则原样(t *testing.T) {
	// Node 实跑: 两条原样保留(引用也应不变)
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "R"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
	}
	out := fixToolAdjacency(in)
	if len(out) != 2 {
		t.Fatalf("配对紧邻应原样保留 2 条, got %d", len(out))
	}
	// 未改动时应返回**原对象**(原版 :891 `result.push(msg)`)
	if _, same := out[0].(map[string]any); !same {
		t.Fatal("首条应为 map")
	}
	blocks := mustJSON(t, out[0])["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("tool_use 应保留, got %s", adjJSON(t, out[0]))
	}
}

func TestFixToolAdjacency_OpenAI形态紧邻(t *testing.T) {
	in := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "function": map[string]any{"name": "R"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
	}
	out := fixToolAdjacency(in)
	if len(out) != 2 {
		t.Fatalf("OpenAI 形态紧邻应保留 2 条, got %d", len(out))
	}
	tcs := mustJSON(t, out[0])["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 应保留, got %s", adjJSON(t, out[0]))
	}
}

func TestFixToolAdjacency_OpenAI形态结果不紧邻(t *testing.T) {
	// 中间隔了 user 文本 -> tool_calls 被修剪; assistant 无正文 -> 整条丢
	in := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "function": map[string]any{"name": "R"}},
		}},
		map[string]any{"role": "user", "content": "mid"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
	}
	out := fixToolAdjacency(in)
	if len(out) != 2 {
		t.Fatalf("应剩 2 条, got %d: %s", len(out), adjJSON(t, out))
	}
}

func TestFixToolAdjacency_单条早退(t *testing.T) {
	// :830 `if (messages.length <= 1) return messages;`
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1"},
		}},
	}
	out := fixToolAdjacency(in)
	if len(out) != 1 {
		t.Fatalf("单条应早退原样, got %d", len(out))
	}
}

func TestFixToolAdjacency_无id的tool_use保留(t *testing.T) {
	// :862 `block.type !== "tool_use" || !block.id || ...`
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use"}, // 无 id
			map[string]any{"type": "tool_use", "id": "ghost"},
		}},
		map[string]any{"role": "user", "content": "no results"},
	}
	out := fixToolAdjacency(in)
	blocks := mustJSON(t, out[0])["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("无 id 的 tool_use 应保留、有 id 无配对的应删, got %s", adjJSON(t, out[0]))
	}
}

// --- stripTrailingAssistantOrphanToolUse ---

func TestStripTrailing_保留正文只删tool_use(t *testing.T) {
	// Node: [user(hi), assistant([text "thinking"])]
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "thinking"},
			map[string]any{"type": "tool_use", "id": "t1"},
		}},
	}
	out := stripTrailingAssistantOrphanToolUse(in)
	if len(out) != 2 {
		t.Fatalf("应保留 2 条, got %d", len(out))
	}
	blocks := mustJSON(t, out[1])["content"].([]any)
	if len(blocks) != 1 || mustJSON(t, blocks[0])["type"] != "text" {
		t.Fatalf("只应剩 text 块, got %s", adjJSON(t, out[1]))
	}
}

func TestStripTrailing_只剩tool_use则整条丢弃(t *testing.T) {
	// Node: [user(hi)]
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1"},
		}},
	}
	out := stripTrailingAssistantOrphanToolUse(in)
	if len(out) != 1 || mustJSON(t, out[0])["role"] != "user" {
		t.Fatalf("只剩 tool_use 的尾巴应被丢弃, got %s", adjJSON(t, out))
	}
}

func TestStripTrailing_tool_calls清空为数组而非删除(t *testing.T) {
	// Node: [user(hi), assistant(content "text", tool_calls [])] —— 键仍在, 值为空数组
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "text", "tool_calls": []any{
			map[string]any{"id": "c1"},
		}},
	}
	out := stripTrailingAssistantOrphanToolUse(in)
	if len(out) != 2 {
		t.Fatalf("有正文应保留, got %d", len(out))
	}
	v, has := mustJSON(t, out[1])["tool_calls"]
	if !has {
		t.Fatal("tool_calls 键应保留(置空数组)")
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("tool_calls 应为空数组, got %v", v)
	}
}

func TestStripTrailing_幂等(t *testing.T) {
	// :951 `if (!modified) return messages;` —— 两种干净形态都应原样返回
	// Node: H 与 I 两个场景都原样。
	userTail := []any{map[string]any{"role": "user", "content": "hi"}}
	if got := stripTrailingAssistantOrphanToolUse(userTail); len(got) != 1 {
		t.Fatalf("user 结尾应原样, got %s", adjJSON(t, got))
	}
	textTail := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "text"},
	}
	if got := stripTrailingAssistantOrphanToolUse(textTail); len(got) != 2 {
		t.Fatalf("纯文本 assistant 结尾应原样, got %s", adjJSON(t, got))
	}
	empty := []any{}
	if got := stripTrailingAssistantOrphanToolUse(empty); len(got) != 0 {
		t.Fatalf("空切片应原样, got %s", adjJSON(t, got))
	}
}

// --- stripTrailingAssistantForProvider ---

func TestStripTrailingForProvider_仅mistral生效(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "text"},
	}
	// mistral: 删掉纯文本 assistant 尾巴
	if got := stripTrailingAssistantForProvider(msgs, "mistral"); len(got) != 1 {
		t.Fatalf("mistral 应删掉纯文本 assistant 尾巴, got %s", adjJSON(t, got))
	}
	// 其他 provider: 原样(Anthropic/OpenAI 允许 assistant 结尾)
	for _, p := range []string{"anthropic", "openai", "tokenrouter", ""} {
		if got := stripTrailingAssistantForProvider(msgs, p); len(got) != 2 {
			t.Fatalf("%q 不应改动, got %s", p, adjJSON(t, got))
		}
	}
}

func TestStripTrailingForProvider_有tool_use时不处理(t *testing.T) {
	// :991-997 有 tool_use / tool_calls 时不动 —— 那是
	// stripTrailingAssistantOrphanToolUse 的职责(必须先跑)。
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1"},
		}},
	}
	if got := stripTrailingAssistantForProvider(msgs, "mistral"); len(got) != 2 {
		t.Fatalf("含 tool_use 时不应处理, got %s", adjJSON(t, got))
	}
	msgs2 := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
	}
	if got := stripTrailingAssistantForProvider(msgs2, "mistral"); len(got) != 2 {
		t.Fatalf("含 tool_calls 时不应处理, got %s", adjJSON(t, got))
	}
}

// --- 接入级: providers_chat.go:chatWithKey 的五步链 ---

// applyToolGuardChain 复刻 chatWithKey 里的接线(顺序与作用域逐字同源),
// 用于在不发 HTTP 的前提下对"接线"做回归。
//
// ★ 作用域照抄: 非 anthropic 时整条链**不执行**, 原样返回。
//
//	参考实现把它包在 Claude-only 的 if 里(executors/base.ts:955-960)。
func applyToolGuardChain(msgs []any, apiType, provider string) []any {
	if apiType != "anthropic" {
		return msgs
	}
	fixed, _ := sanitizeClaudeToolIDs(msgs)
	fixed = splitMisplacedToolResults(fixed)
	fixed = fixToolUseOrdering(fixed)
	fixed = fixToolPairs(fixed)
	fixed = fixToolPairs(fixToolAdjacency(fixed))
	fixed = stripTrailingAssistantOrphanToolUse(fixed)
	return stripTrailingAssistantForProvider(fixed, normalizeProviderID(provider))
}

func TestChatWithKey接入_trailing调用被清理(t *testing.T) {
	// 场景: 客户端发来的历史以 assistant(tool_use) 结尾 —— 上游发送前必须修掉。
	// 这是 Codex / Cline 这类 agent 客户端"截断历史"时的典型形态。
	in := []any{
		map[string]any{"role": "user", "content": "read the file"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "Read", "arguments": `{"p":"a"}`}},
		}},
	}
	got := applyToolGuardChain(in, "anthropic", "anthropic")
	// stripTrailingAssistantOrphanToolUse 删掉尾部 tool_calls;
	// 该 assistant 无 content -> 整条丢弃; 于是只剩 user。
	if len(got) != 1 {
		t.Fatalf("尾部孤儿调用应被清理至 1 条, got %d: %s", len(got), adjJSON(t, got))
	}
	if mustJSON(t, got[0])["role"] != "user" {
		t.Fatalf("应只剩 user, got %s", adjJSON(t, got))
	}
}

func TestChatWithKey接入_Anthropic走相邻性_OpenAI不走(t *testing.T) {
	// 同一份输入, 仅 apiType 不同 -> 结果必须不同, 这是照抄参考实现
	// `const isClaude = this.provider === "claude" || usesClaudeCodeProtocol`
	// 的直接体现: OpenAI 允许 tool_result 分散在多条后续消息。
	//
	// 注意 openai 一栏在本例里"三条原样"(不是"被修成两条")—— 因为整条链
	// 包在 anthropic 分支里, 非 anthropic 时**一步都不跑**。这同时锁住了
	// "作用域"这一照抄要点(base.ts:955-960 的 Claude-only 前提)。
	in := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "R"},
		}},
		map[string]any{"role": "user", "content": "intervening text"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
	}

	// anthropic 的权威值是「两条」—— 这是**六步链**(含 Pass 1.45 + Pass 1.5)的结果:
	//
	//   Pass 1.45 splitMisplacedToolResults: 本输入里没有 assistant 带 tool_result,
	//     故 no-op(三条原样)。
	//   Pass 1.5  fixToolUseOrdering: Pass 2 把相邻的两条 user 合并,
	//     tool_result 提前 -> user 变成 [tool_result(t1), text("intervening text")]。
	//     同时 Pass 3 认出 t1 正是紧邻上一条 assistant 的 tool_use id -> 保持结构化,
	//     **不降级**为文本。
	//   后续 fixToolPairs / fixToolAdjacency / stripTrailing*: 全部 no-op
	//     (assistant 的 tool_use 已有紧邻配对, 尾部也不是 assistant)。
	//
	// ★ 关键: 这条链把「不紧邻」修成了「紧邻」, 而不是删掉它 —— 这正是
	//   fixToolUseOrdering 相对旧链的增益: 旧链(fixToolAdjacency)只会把
	//   tool_result 够不着的 tool_use 剥掉、再把孤儿 tool_result 清掉, 于是
	//   整个工具往返消失(剩下"intervening text"), 模型再也看不到工具输出。
	//   新链把它归一成合法形状, 工具输出得以保留。
	//
	// 权威值由 Node 实跑参考实现的六个函数取得(见 .negbak/probe_chain6.mjs,
	// 同输入同顺序的 C1/C3 两栏对照), Go 侧输出与之一致(键序不同, 逐字段等价)。
	anthropicOut := applyToolGuardChain(in, "anthropic", "anthropic")
	if len(anthropicOut) != 2 {
		t.Fatalf("anthropic 路径应剩 2 条, got %d: %s",
			len(anthropicOut), adjJSON(t, anthropicOut))
	}
	if mustJSON(t, anthropicOut[0])["role"] != "assistant" {
		t.Fatalf("首条应是 assistant(tool_use), got %s", adjJSON(t, anthropicOut))
	}
	merged := mustJSON(t, anthropicOut[1])
	if merged["role"] != "user" {
		t.Fatalf("次条应是 user, got %s", adjJSON(t, anthropicOut))
	}
	// 合并后的 user: [tool_result(t1), text("intervening text")] —— tool_result 提前。
	blocks, ok := merged["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("合并后的 user 应有 2 块, got %s", adjJSON(t, anthropicOut))
	}
	b0 := mustJSON(t, blocks[0])
	if b0["type"] != "tool_result" || b0["tool_use_id"] != "t1" {
		t.Fatalf("首块应是 tool_result(t1), got %s", b0)
	}
	b1 := mustJSON(t, blocks[1])
	if b1["type"] != "text" || b1["text"] != "intervening text" {
		t.Fatalf("次块应是 text(\"intervening text\"), got %s", b1)
	}

	// openai: 整条链不执行 -> 三条原样保留。
	// 这锁的是"作用域"本身: 曾有一版无条件挂上, 结果破坏 gemini/openai 路径。
	openaiOut := applyToolGuardChain(in, "openai", "openai")
	if len(openaiOut) != 3 {
		t.Fatalf("非 anthropic 路径应完全不改动, got %d: %s", len(openaiOut), adjJSON(t, openaiOut))
	}
}

func TestChatWithKey接入_干净历史幂等(t *testing.T) {
	// 参考实现原注释: "Both are idempotent on clean histories."
	// 一条正常往返的对话不应被这条链改动。
	in := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "R", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
		map[string]any{"role": "assistant", "content": "done"},
	}
	// 只对 anthropic 有意义: 非 anthropic 时整条链不执行, 恒等不构成证据。
	got := applyToolGuardChain(in, "anthropic", "anthropic")
	if len(got) != len(in) {
		t.Fatalf("anthropic 路径干净历史应幂等, got %d 条: %s", len(got), adjJSON(t, got))
	}
}

func TestChatWithKey接入_mistral尾巴(t *testing.T) {
	// stripTrailingAssistantForProvider 只认 mistral(照抄 :972 白名单)。
	in := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "plain text"},
	}
	if got := applyToolGuardChain(in, "anthropic", "mistral"); len(got) != 1 {
		t.Fatalf("mistral 应删纯文本 assistant 尾巴, got %s", adjJSON(t, got))
	}
	if got := applyToolGuardChain(in, "anthropic", "openai"); len(got) != 2 {
		t.Fatalf("非 mistral 不应改动, got %s", adjJSON(t, got))
	}
}

// TestChatWithKey接线_源码层锁死 用源码文本断言防止"复刻测试与真实接线脱节"。
//
// 背景: 上面的 applyToolGuardChain 是**复刻**接线, 因此改坏 providers_chat.go
// 不会让它变红 —— 这类测试只能证明"逻辑对", 证明不了"接线在"。
// 本用例补上那一半: 直接检查真实调用点里各函数齐全、且顺序正确。
//
// 这是**文本级**断言, 故意做得笨拙而精确: 照抄纪律要求接线可被机械核对。
func TestChatWithKey接线_源码层锁死(t *testing.T) {
	src, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读 providers_chat.go 失败: %v", err)
	}
	s := string(src)

	// 各函数必须都出现在接线块里
	for _, fn := range []string{
		"sanitizeClaudeToolIDs(",
		"splitMisplacedToolResults(",
		"fixToolUseOrdering(",
		"fixToolPairs(",
		"fixToolAdjacency(",
		"stripTrailingAssistantOrphanToolUse(",
		"stripTrailingAssistantForProvider(",
	} {
		if !strings.Contains(s, fn) {
			t.Fatalf("接线缺失: %s 未在 providers_chat.go 中被调用", fn)
		}
	}

	// 顺序必须与参考实现一致:
	//   sanitizeClaudeToolIDs 照抄 claudeHelper.ts:432-450 (Pass 1.4, id 双侧净化),
	//   splitMisplacedToolResults 照抄 claudeHelper.ts:103-156 (#2815, Pass 1.45),
	//   fixToolUseOrdering 照抄 claudeHelper.ts:160-284 (Pass 1.5),
	//   其余四步照抄 services/contextManager.ts:717-1000 (base.ts:1320-1339)。
	//
	// ★ 前两步的相对顺序是**照抄的**, 不是随意排的: 参考实现先做 Pass 1.4(净化 id)
	//   再做 Pass 1.45(搬块), 因为 Pass 1.45 的"孤儿 tool_result 丢弃"判定要拿
	//   净化后的 id 表去比对。顺序颠倒会让该判定基于过期的 id 表。
	//
	// ★ fixToolUseOrdering 的插入点同样是照抄的: 参考实现在 prepareClaudeRequest
	//   里按 Pass 1.4 → Pass 1.45 → Pass 1.5 排列(claudeHelper.ts:422/484/488)。
	//   它必须晚于 splitMisplacedToolResults(块要先归位, Pass 3 的配对判定才准),
	//   早于 fixToolPairs/fixToolAdjacency(它已把孤儿转文本+补占位, 后续应为 no-op)。
	//
	// ★ 整条链必须包在 `if cfg.APIType == "anthropic" {` 里 —— 参考实现同样把它
	//   包在 Claude-only 的 if 里(base.ts:955-960), 越界施加会破坏其它协议。
	order := []string{
		`if cfg.APIType == "anthropic" {`,
		"fixed, idChanged := sanitizeClaudeToolIDs(msgs)",
		"fixed = splitMisplacedToolResults(fixed)",
		"fixed = fixToolUseOrdering(fixed)",
		"fixed = fixToolPairs(fixed)",
		"fixed = fixToolPairs(fixToolAdjacency(fixed))",
		"fixed = stripTrailingAssistantOrphanToolUse(fixed)",
		"params[\"messages\"] = stripTrailingAssistantForProvider(fixed,",
	}
	pos := -1
	for _, frag := range order {
		i := strings.Index(s, frag)
		if i < 0 {
			t.Fatalf("接线片段缺失: %q", frag)
		}
		if i <= pos {
			t.Fatalf("接线顺序错误: %q 出现在前一片段之前", frag)
		}
		pos = i
	}
}
