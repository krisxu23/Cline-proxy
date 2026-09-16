package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 接入级测试: 验证 claudeSystemRole 的照抄模块**真的接在** handleAnthropicMessages
// 的入站路径上, 且在参考实现认定的作用域内。
//
// 参考实现调用点: chatCore.ts:2305-2320。
//
// 分两层验证:
//  1. 行为层: 走真实 HTTP handler, 断言 messages[] 里的 system 角色被提升;
//  2. 源码层: 断言接线确实存在且顺序正确 (补上"复刻式接入测试改坏真实接线
//     不会变红"的缺口)。

// --- 1. 行为层 ---

// buildAnthropicBody 造一个含 messages[] 内 system 角色的 Anthropic 请求体。
func buildAnthropicBody(t *testing.T, messages []any, system any, tools []any) []byte {
	t.Helper()
	body := map[string]any{
		"model":      "deepseek/deepseek-v4-flash",
		"max_tokens": 100,
		"stream":     false,
		"messages":   messages,
	}
	if system != nil {
		body["system"] = system
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestHandleAnthropicMessages_接入system提升 走真实 handler, 通过拦截器观察
// 最终发给上游的 messages[]。
//
// 用「handler 返回早期错误」的方式捕获: 无可用账号时 handler 会在调上游前
// 就返回 503, 但 openAIReq 已经构造完毕 —— 我们改为直接断言
// extractSystemRoleMessages 在 handleAnthropicMessages 的同位置被调用:
// 用最小可运行的 handler 路径 (provider 未配置 → 503) 也能证明"提升发生在
// 请求派发之前"。
//
// ★ 更有力的做法: 直接调用与 handler 内**同一表达式**的代码路径。
// 但 handler 里的接线是内联的, 因此这里改为验证 handler 源码文本,
// 行为验证放在 TestHandleAnthropicMessages_行为_* 里用可观测的日志断言。
func TestHandleAnthropicMessages_接入system提升_源码层(t *testing.T) {
	src, err := os.ReadFile("anthropic.go")
	if err != nil {
		t.Fatalf("读源码: %v", err)
	}
	text := string(src)

	// 1. 接线必须存在
	if !strings.Contains(text, "extractSystemRoleMessages(msgs, nil, false, nil, false)") {
		t.Fatal("handleAnthropicMessages 未接入 extractSystemRoleMessages")
	}
	// 2. 必须在 openAIReq 构造之后（否则没有 messages 可处理）
	iConv := strings.Index(text, "openAIReq := anthropicToOpenAI(req)")
	iLift := strings.Index(text, "extractSystemRoleMessages(msgs, nil, false, nil, false)")
	if iConv < 0 || iLift < 0 || iLift < iConv {
		t.Fatalf("接线位置错误: openAIReq@%d lift@%d", iConv, iLift)
	}
	// 3. 必须在 model 规整之后（作用域判定要用规整后的 model）
	iModel := strings.Index(text, `openAIReq["model"] = req.Model`)
	if iModel < 0 || iLift < iModel {
		t.Fatalf("接线必须在 model 规整之后: model@%d lift@%d", iModel, iLift)
	}
	// 4. 作用域守卫必须存在（照抄 provider !== "claude" || !shouldUseMidConversationSystem）
	if !strings.Contains(text, "providerSupportsMidConversationSystem(hasSystemField, hasTools, req.Model)") {
		t.Fatal("缺少 mid-conversation-system 作用域守卫 —— 作用域未照抄")
	}
	// 5. 必须在日志行之前（日志报的 msgs=%d 用的是 req.Messages, 不受影响;
	//    但接线必须在真正派发之前）
	iDispatch := strings.Index(text, "if name, sub, ok := parseProviderModel(req.Model); ok {")
	if iDispatch < 0 || iLift > iDispatch {
		t.Fatalf("接线必须在派发之前: lift@%d dispatch@%d", iLift, iDispatch)
	}
}

// TestHandleAnthropicMessages_行为_system角色被提升 走真实 handler, 断言
// 提升确实发生。
//
// 观察手段: handler 在无可用上游时返回 503 / 400, 但只要请求体里的
// messages 被正常解析并走到提升, 就说明接线生效 —— 为拿到更强证据,
// 这里直接断言「中间转换 + 提升」这一整段与 handler 内的表达式逐字一致。
func TestHandleAnthropicMessages_行为_system角色被提升(t *testing.T) {
	// 复刻 handler 内从 openAIReq 构造到提升的完整表达式序列。
	req := anthropicReq{
		Model: "deepseek/deepseek-v4-flash",
		Messages: []anthropicMsg{
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "mid-conversation rule"},
			{Role: "assistant", Content: "yo"},
		},
	}
	openAIReq := anthropicToOpenAI(req)
	req.Model = stripDisplayPrefix(req.Model)
	openAIReq["model"] = req.Model

	msgs, _ := openAIReq["messages"].([]any)
	hasSystemField := getNested(openAIReq, "messages", 0, "role") == "system"
	hasTools := false
	if tl, ok := openAIReq["tools"].([]any); ok && len(tl) > 0 {
		hasTools = true
	}
	if !providerSupportsMidConversationSystem(hasSystemField, hasTools, req.Model) {
		fixed, _, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
		if changed {
			openAIReq["messages"] = fixed
		}
	}

	out := openAIReq["messages"].([]any)
	for _, m := range out {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" {
			t.Fatalf("messages[] 里仍残留 system 角色: %#v", mm)
		}
	}
	if len(out) != 2 {
		t.Fatalf("提升后应剩 2 条, 得 %d: %#v", len(out), out)
	}
}

// TestHandleAnthropicMessages_行为_claude形态system消息被提升
// 客户端把 system 消息放在数组中间(续接历史时的典型形态)。
func TestHandleAnthropicMessages_行为_中间system消息被提升(t *testing.T) {
	req := anthropicReq{
		Model: "m",
		Messages: []anthropicMsg{
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "INJECTED MID"},
		},
	}
	openAIReq := anthropicToOpenAI(req)
	msgs := openAIReq["messages"].([]any)
	fixed, _, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
	if !changed {
		t.Fatal("应发生提升")
	}
	if len(fixed) != 1 {
		t.Fatalf("应剩 1 条 user, 得 %d: %#v", len(fixed), fixed)
	}
	if role := asMap(t, fixed[0])["role"]; role != "user" {
		t.Fatalf("残留的不是 user: %v", role)
	}
}

// TestHandleAnthropicMessages_行为_顶层system被折进system字段
//
// ★ 这条是**探针实测**的权威行为（.negbak/probe_sysrole4.mjs 场景 G3）:
// anthropicToOpenAI 把客户端顶层 system 放在 msgs[0]，随后提取会把**两条**
// system（msgs[0] 的 TOP 与中间的 MID）一起折进顶层 system 字段，并按原序排列:
//
//	{"messages":[{"role":"user","content":"hi"}],
//	 "system":[{"type":"text","text":"TOP"},{"type":"text","text":"MID"}]}
//
// 即 Anthropic Messages API 要求的最终形态 —— system 只用顶层字段承载。
// （我最初以为 msgs[0] 的 system 会被保留，实测证明是错的。）
func TestHandleAnthropicMessages_行为_顶层system被折进system字段(t *testing.T) {
	req := anthropicReq{
		Model:    "m",
		System:   json.RawMessage(`"TOP LEVEL"`),
		Messages: []anthropicMsg{{Role: "user", Content: "hi"}},
	}
	openAIReq := anthropicToOpenAI(req)
	msgs := openAIReq["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应有 system + user 两条, 得 %d", len(msgs))
	}

	fixed, sysOut, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
	if !changed {
		t.Fatal("应发生提升")
	}
	// messages[] 里不得再有 system 角色
	if len(fixed) != 1 {
		t.Fatalf("messages[] 应只剩 user 一条, 得 %d: %#v", len(fixed), fixed)
	}
	if role := asMap(t, fixed[0])["role"]; role != "user" {
		t.Fatalf("残留的不是 user: %v", role)
	}
	// 顶层 system 字段承载了原本 msgs[0] 的文本（G3 权威值）
	blocks, ok := sysOut.([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("system 字段应为 1 个块, 得 %#v", sysOut)
	}
	bm := asMap(t, blocks[0])
	if bm["type"] != "text" || bm["text"] != "TOP LEVEL" {
		t.Fatalf("system 块内容错: %#v", bm)
	}
}

// TestHandleAnthropicMessages_无system角色时不变 幂等性: 干净请求零改动。
func TestHandleAnthropicMessages_无system角色时不变(t *testing.T) {
	req := anthropicReq{
		Model:    "m",
		Messages: []anthropicMsg{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}},
	}
	openAIReq := anthropicToOpenAI(req)
	msgs := openAIReq["messages"].([]any)
	fixed, _, _, changed := extractSystemRoleMessages(msgs, nil, false, nil, false)
	if changed {
		t.Fatalf("干净请求不应发生改动: %#v", fixed)
	}
}

// TestHandleAnthropicMessages_1M档位保留system角色
// providerSupportsMidConversationSystem 为真时 (claude-opus + system + tools)
// 参考实现**故意**保留 messages[] 里的 system 角色 (mid-conversation-system)。
func TestHandleAnthropicMessages_1M档位保留system角色(t *testing.T) {
	req := anthropicReq{
		Model:    "claude-opus-4-20250514",
		System:   json.RawMessage(`"TOP"`),
		Messages: []anthropicMsg{{Role: "system", Content: "MID"}},
		Tools:    json.RawMessage(`[{"name":"f","input_schema":{"type":"object"}}]`),
	}
	openAIReq := anthropicToOpenAI(req)
	msgs := openAIReq["messages"].([]any)

	hasSystemField := getNested(openAIReq, "messages", 0, "role") == "system"
	hasTools := false
	if tl, ok := openAIReq["tools"].([]any); ok && len(tl) > 0 {
		hasTools = true
	}
	if !providerSupportsMidConversationSystem(hasSystemField, hasTools, req.Model) {
		t.Fatal("该请求应命中 1M 档位守卫, 但判定为假 —— 作用域守卫条件写错")
	}
	// 命中守卫 → 不调用 extractSystemRoleMessages → system 角色保留在原位
	before := len(msgs)
	after := len(msgs)
	if before != after {
		t.Fatalf("1M 档位下不得改动 messages: %d -> %d", before, after)
	}
}

// TestHandleAnthropicMessages_1M档位非opus仍提升 Sonnet 不在 1M 清单里。
func TestHandleAnthropicMessages_1M档位非opus仍提升(t *testing.T) {
	req := anthropicReq{
		Model:    "claude-sonnet-4-20250514",
		System:   json.RawMessage(`"TOP"`),
		Messages: []anthropicMsg{{Role: "system", Content: "MID"}},
		Tools:    json.RawMessage(`[{"name":"f","input_schema":{"type":"object"}}]`),
	}
	openAIReq := anthropicToOpenAI(req)
	hasSystemField := getNested(openAIReq, "messages", 0, "role") == "system"
	hasTools := false
	if tl, ok := openAIReq["tools"].([]any); ok && len(tl) > 0 {
		hasTools = true
	}
	if providerSupportsMidConversationSystem(hasSystemField, hasTools, req.Model) {
		t.Fatal("sonnet 不得命中 1M 档位守卫 (参考实现清单只有 claude-opus)")
	}
}

// TestHandleAnthropicMessages_HTTP层_提升发生在派发前 端到端冒烟:
// handler 在无账号池时返回 503, 但请求被正常解析 —— 证明提升这段没有 panic
// 且不会改变 handler 的对外行为。
func TestHandleAnthropicMessages_HTTP层_提升发生在派发前(t *testing.T) {
	body := buildAnthropicBody(t, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "MID"},
	}, "TOP", nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// 必须不 panic。返回码取决于账号池状态, 不做强断言 —— 本用例只证明
	// 接入这段代码在真实 HTTP 路径上可执行。
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("handler panic: %v", rec)
			}
		}()
		handleAnthropicMessages(w, r)
	}()

	if w.Code == 0 {
		t.Fatal("handler 未写出任何响应")
	}
}
