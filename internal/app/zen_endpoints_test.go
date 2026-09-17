package app

// zen 出站端点选择的测试(2026-09-17 补齐 /messages 端点)。
//
// 背景: 官方文档给出 4 种端点, 7 个免费模型跨 3 个。此前出站只有
// /chat/completions 与 /responses 两个分支, 于是 union-alpha(免费, 只在
// /messages)会被发到 /chat/completions 必然失败。
//
// 端点矩阵与依据见 docs/opencode-zen-facts.md 第 1、2 节。

import (
	"path/filepath"
	"testing"
)

// TestZenEndpointPathFor 端点相对路径。
//
// ★ 这里锁的是那个真实的坑: base 已含 /v1, 而官方文档里 /messages 写的是
// 完整路径 .../zen/v1/messages —— 所以相对路径必须是 "/messages"。
// 写成 "/v1/messages" 会拼出 .../zen/v1/v1/messages。
func TestZenEndpointPathFor(t *testing.T) {
	cases := []struct {
		kind  zenEndpointKind
		model string
		want  string
	}{
		{zenEndpointChat, "mimo-v2.5-free", "/chat/completions"},
		{zenEndpointResponses, "muse-spark-1.3-contributor-free", "/responses"},
		{zenEndpointMessages, "union-alpha", "/messages"},
		{zenEndpointGemini, "gemini-3.8-flash", "/models/gemini-3.8-flash"},
	}
	for _, c := range cases {
		if got := c.kind.pathFor(c.model); got != c.want {
			t.Fatalf("%v.pathFor(%q) = %q, 期望 %q", c.kind, c.model, got, c.want)
		}
	}
	// 反向断言: 绝不能出现 /v1/messages(base 已含 /v1)
	if got := zenEndpointMessages.pathFor("x"); got == "/v1/messages" {
		t.Fatal("相对路径不得带 /v1 —— base 已含, 会拼出 .../zen/v1/v1/messages")
	}
}

// TestZenStaticEndpoint 官方文档端点矩阵里**有依据**的条目。
func TestZenStaticEndpoint(t *testing.T) {
	cases := []struct {
		model     string
		wantKind  zenEndpointKind
		wantKnown bool
	}{
		// 实测: chat/completions 被后端崩成 500
		{"muse-spark-1.3-contributor-free", zenEndpointResponses, true},
		{"muse-spark-1.2-free", zenEndpointResponses, true},
		// 官方文档明确只在 /messages
		{"union-alpha", zenEndpointMessages, true},
		{"Union-Alpha", zenEndpointMessages, true}, // 大小写不敏感
		// ★ 以下一律"不介入": 官方端点矩阵描述的是官方客户端走哪条路,
		//   不等于只有那条路能通 —— 按前缀批量改路由可能把能用的付费模型改坏。
		{"claude-opus-5", zenEndpointChat, false},
		{"qwen3.7-max", zenEndpointChat, false},
		{"gpt-5.5", zenEndpointChat, false},
		{"grok-4.6", zenEndpointChat, false},
		{"gemini-3.8-flash", zenEndpointChat, false},
		{"mimo-v2.5-free", zenEndpointChat, false},
		{"big-pickle", zenEndpointChat, false},
		{"nemotron-3-ultra-free", zenEndpointChat, false},
		{"", zenEndpointChat, false},
	}
	for _, c := range cases {
		kind, known := zenStaticEndpoint(c.model)
		if kind != c.wantKind || known != c.wantKnown {
			t.Fatalf("zenStaticEndpoint(%q) = (%v, %v), 期望 (%v, %v)",
				c.model, kind, known, c.wantKind, c.wantKnown)
		}
	}
}

// TestZenEndpointFor_优先级 学习结果 > 静态表 > 默认 chat。
func TestZenEndpointFor_优先级(t *testing.T) {
	origFile := zenMessagesOnlyFileOverride
	tmp := filepath.Join(t.TempDir(), "zen-messages-only.json")
	setZenMessagesOnlyFileForTest(tmp)
	t.Cleanup(func() {
		setZenMessagesOnlyFileForTest(origFile)
		zenMsgOnlyMu.Lock()
		zenMsgOnly = map[string]bool{}
		zenMsgOnlyLoaded = false
		zenEndpointChatOnlyMemo = map[string]bool{}
		zenMsgOnlyMu.Unlock()
	})

	// 1. 静态表
	if k := zenEndpointFor("union-alpha"); k != zenEndpointMessages {
		t.Fatalf("union-alpha 应走 messages, 实得 %v", k)
	}
	if k := zenEndpointFor("muse-spark-1.3-contributor-free"); k != zenEndpointResponses {
		t.Fatalf("muse-*-free 应走 responses, 实得 %v", k)
	}
	// 2. 默认
	if k := zenEndpointFor("mimo-v2.5-free"); k != zenEndpointChat {
		t.Fatalf("未知模型应默认 chat, 实得 %v", k)
	}
	// 3. 学习结果压过默认
	if k := zenEndpointFor("learned-model"); k != zenEndpointChat {
		t.Fatalf("学习前应默认 chat, 实得 %v", k)
	}
	zenLearnMessagesOnly("learned-model")
	if k := zenEndpointFor("learned-model"); k != zenEndpointMessages {
		t.Fatalf("学习后应走 messages, 实得 %v", k)
	}
	// 4. 空模型名不 panic
	if k := zenEndpointFor(""); k != zenEndpointChat {
		t.Fatalf("空模型名应默认 chat, 实得 %v", k)
	}
}

// TestZenLearnMessagesOnly_持久化 成功调通的模型要落盘, 重启后仍生效。
func TestZenLearnMessagesOnly_持久化(t *testing.T) {
	origFile := zenMessagesOnlyFileOverride
	tmp := filepath.Join(t.TempDir(), "zen-messages-only.json")
	setZenMessagesOnlyFileForTest(tmp)
	t.Cleanup(func() {
		setZenMessagesOnlyFileForTest(origFile)
		zenMsgOnlyMu.Lock()
		zenMsgOnly = map[string]bool{}
		zenMsgOnlyLoaded = false
		zenMsgOnlyMu.Unlock()
	})

	zenLearnMessagesOnly("persist-test-model")

	// 模拟重启: 清空内存态, 从文件重新加载
	zenMsgOnlyMu.Lock()
	zenMsgOnly = map[string]bool{}
	zenMsgOnlyLoaded = false
	zenMsgOnlyMu.Unlock()

	if !zenMessagesOnlyKnown("persist-test-model") {
		t.Fatal("学习结果应已持久化, 重启后仍应命中")
	}
}

// TestZenEndpointChatOnlyMemo 负向结论**只进程内记, 不落盘**。
//
// 纪律(沿用 zen_responses.go): 上游随时可能修复端点支持, 写死了就永远错了。
func TestZenEndpointChatOnlyMemo(t *testing.T) {
	if zenEndpointChatOnlyKnown("neg-test") {
		t.Fatal("初始应为 false")
	}
	zenMemoEndpointChatOnly("neg-test")
	if !zenEndpointChatOnlyKnown("neg-test") {
		t.Fatal("记负向后应为 true")
	}
	// 负向不影响 zenEndpointFor 的正向判定
	if k := zenEndpointFor("neg-test"); k != zenEndpointChat {
		t.Fatalf("负向只用于「不再回退」, 不改端点判定, 实得 %v", k)
	}
}

// TestZenEndpointAuth Anthropic Messages 端点要用 Anthropic 方言的鉴权。
func TestZenEndpointAuth(t *testing.T) {
	if !zenEndpointMessages.usesAnthropicAuth() {
		t.Fatal("/messages 应用 x-api-key + anthropic-version")
	}
	for _, k := range []zenEndpointKind{zenEndpointChat, zenEndpointResponses, zenEndpointGemini} {
		if k.usesAnthropicAuth() {
			t.Fatalf("%v 应走 Bearer, 不是 Anthropic 方言", k)
		}
	}
}

// TestZenEndpointFallbacks 回退候选应包含除首选外的全部形态。
func TestZenEndpointFallbacks(t *testing.T) {
	got := zenEndpointFallbacks(zenEndpointChat)
	if len(got) != 2 {
		t.Fatalf("chat 应有 2 个回退候选, 实得 %d: %v", len(got), got)
	}
	for _, k := range got {
		if k == zenEndpointChat {
			t.Fatal("回退候选不应包含首选自身")
		}
	}
}
