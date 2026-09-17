package app

import (
	"os"
	"strings"
	"testing"
)

// 本文件是**源码层接线锁**：复刻测试只能证明"函数实现对不对"，证明不了
// "它有没有被接进生产路径"。这里用 os.ReadFile + strings.Index/Count 直接检查
// 真实源码文本，锁死调用点的**次数、位置、顺序与作用域边界**。
//
// ★ 教训一（本轮抓出）：只断言"第一次出现的位置落在正确区间"不够 ——
//   把调用**同时**放进两个分支也骗得过单点 Index。必须断言**调用次数 == 1**。
// ★ 教训二（本轮抓出）：照抄的值若语义上在判别的**不是同一件事**，即便当前
//   恒等也要问清（见 claude_thinking_blocks 的 modelTargetsClaude 事件）。

func readAppSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return string(b)
}

// countToken 统计子串出现次数。
func countToken(hay, needle string) int {
	return strings.Count(hay, needle)
}

// idxOrFail 返回子串首次出现的下标，未找到则 FAIL。
func idxOrFail(t *testing.T, hay, needle, label string) int {
	t.Helper()
	i := strings.Index(hay, needle)
	if i < 0 {
		t.Fatalf("%s: 未找到 %q", label, needle)
	}
	return i
}

// TestToolRemap接线_providers_chat源码层锁死 锁请求侧 cloak 的接入。
//
// 参考实现位置: executors/base.ts:955-970。
func TestToolRemap接线_providers_chat源码层锁死(t *testing.T) {
	src := readAppSrc(t, "providers_chat.go")

	// 1) 两个调用各出现**恰好 1 次**（防"同时放进两个分支"）。
	if got := countToken(src, "remapToolNamesInRequest(params)"); got != 1 {
		t.Fatalf("remapToolNamesInRequest(params) 出现 %d 次, 期望 1", got)
	}
	if got := countToken(src, "cloakThirdPartyToolNames(params, nil)"); got != 1 {
		t.Fatalf("cloakThirdPartyToolNames(params, nil) 出现 %d 次, 期望 1", got)
	}

	// 2) 必须落在 anthropic 分支内、且在 reanchorClaudePromptCache 之前。
	anchorIf := idxOrFail(t, src, `if cfg.APIType == "anthropic" {`, "anthropic 分支")
	idxRemap := idxOrFail(t, src, "remapToolNamesInRequest(params)", "remap")
	idxCloak := idxOrFail(t, src, "cloakThirdPartyToolNames(params, nil)", "cloak")
	idxReanchor := idxOrFail(t, src, "reanchorClaudePromptCache(params,", "reanchor")

	if idxRemap <= anchorIf {
		t.Fatalf("remap 调用(%d) 必须晚于 anthropic 分支开启(%d)", idxRemap, anchorIf)
	}
	if idxCloak <= idxRemap {
		t.Fatalf("cloak(%d) 必须晚于 remap(%d) —— 顺序是照抄的", idxCloak, idxRemap)
	}
	if idxCloak >= idxReanchor {
		t.Fatalf("cloak(%d) 必须早于 reanchor(%d)", idxCloak, idxReanchor)
	}

	// 3) 不得出现在任何**非 anthropic** 的作用域（越界施加）。
	//    判据: cloak 调用必须早于 else 分支的 applyEmptyReasoningReplay。
	idxElse := idxOrFail(t, src, "applyEmptyReasoningReplay(", "else 分支")
	if idxCloak >= idxElse {
		t.Fatalf("cloak(%d) 必须早于 OpenAI 形态分支(%d) —— 不得外溢", idxCloak, idxElse)
	}
}

// TestToolRemap接线_detach在marshal前 锁 "_toolNameMap 绝不上行"。
func TestToolRemap接线_detach在marshal前(t *testing.T) {
	src := readAppSrc(t, "providers_chat.go")

	if got := countToken(src, "detachToolNameMap(params)"); got != 1 {
		t.Fatalf("detachToolNameMap(params) 出现 %d 次, 期望 1", got)
	}
	idxDetach := idxOrFail(t, src, "detachToolNameMap(params)", "detach")
	// 必须用**真实调用形态**定位 marshal —— 裸 token `json.Marshal(` 会先命中
	// 上方注释里的同一串文字（本轮已实测踩到）。
	//
	// ★ 2026-09-17: marshal 的目标从 params 变成 outbound —— 新增了 APIFormat
	// 协议形态转换, chat 形态下 outbound **就是** params 本身(原样返回), 因此
	// 序列化内容不变, 但顺序约束多了一条: 旁路键回填必须在 marshal **之后**。
	idxMarshal := idxOrFail(t, src, "payload, err := json.Marshal(outbound)", "marshal")
	if idxDetach >= idxMarshal {
		t.Fatalf("detach(%d) 必须在 marshal(%d) 之前", idxDetach, idxMarshal)
	}

	// 旁路键必须在 marshal 前被 delete（否则它成了 body 里的一个额外字段）。
	if got := countToken(src, "delete(params, toolNameMapSideChannelKey)"); got != 1 {
		t.Fatalf("delete(params, toolNameMapSideChannelKey) 出现 %d 次, 期望 1", got)
	}
	idxDelete := idxOrFail(t, src, "delete(params, toolNameMapSideChannelKey)", "delete 旁路键")
	if idxDelete >= idxMarshal {
		t.Fatalf("旁路键 delete(%d) 必须在 marshal(%d) 之前", idxDelete, idxMarshal)
	}

	// ★ 回填必须在 marshal **之后**: chat 形态下 outbound 就是 params 本身,
	// 提前回填会把旁路键序列化进上行 body(Anthropic 会回 400 Extra inputs)。
	idxReadd := idxOrFail(t, src, "params[toolNameMapSideChannelKey] = wireToolNameMap", "回填旁路键")
	if idxReadd <= idxMarshal {
		t.Fatalf("旁路键回填(%d) 必须在 marshal(%d) 之后 —— 否则会进 body", idxReadd, idxMarshal)
	}
}

// TestToolRemap接线_响应侧还原三态 锁三条客户端形态各有一处还原。
//
// 参考实现覆盖三种出站形态（responseTranslator.ts:165/173/740 +
// utils/stream.ts:restoreClaudePassthroughToolUseName）：
//  1. Claude 流式   -> anthropic.go emitToolBlock
//  2. Claude 非流式 -> anthropic.go openAIToAnthropicWithMap
//  3. OpenAI 两态   -> proxy_stream.go 的 restoreOpenAIToolNames
func TestToolRemap接线_响应侧还原三态(t *testing.T) {
	anthSrc := readAppSrc(t, "anthropic.go")

	// 1) Claude 流式: emitToolBlock 里必须调 restoreClaudeToolName(acc.name, toolNameMap)。
	if got := countToken(anthSrc, "restoreClaudeToolName(acc.name, toolNameMap)"); got != 1 {
		t.Fatalf("流式还原 restoreClaudeToolName(acc.name, toolNameMap) 出现 %d 次, 期望 1", got)
	}
	// 必须用 emitName 而不是 acc.name 写进 content_block。
	if got := countToken(anthSrc, `"name":  emitName,`); got != 1 {
		t.Fatalf("`\"name\":  emitName,` 出现 %d 次, 期望 1（content_block 必须写还原后的名字）", got)
	}

	// 2) Claude 非流式: openAIToAnthropicWithMap 里必须调 restoreClaudeToolName(name, nameMap)。
	if got := countToken(anthSrc, "restoreClaudeToolName(name, nameMap)"); got != 1 {
		t.Fatalf("非流式还原 restoreClaudeToolName(name, nameMap) 出现 %d 次, 期望 1", got)
	}

	streamSrc := readAppSrc(t, "proxy_stream.go")

	// 3) OpenAI 两态: 各一处 restoreOpenAIToolNames。
	if got := countToken(streamSrc, "restoreOpenAIToolNames(normalized, toolNameMap)"); got != 1 {
		t.Fatalf("流式 OpenAI 还原出现 %d 次, 期望 1", got)
	}
	if got := countToken(streamSrc, "restoreOpenAIToolNames(out, toolNameMap)"); got != 1 {
		t.Fatalf("非流式 OpenAI 还原出现 %d 次, 期望 1", got)
	}
}

// TestToolRemap接线_实现文件常量齐全 锁照抄的常量表没有被省略。
func TestToolRemap接线_实现文件常量齐全(t *testing.T) {
	src := readAppSrc(t, "claude_tool_remap.go")

	for _, token := range []string{
		"const toolNameMapKey = \"_toolNameMap\"",
		"const toolNameMapSideChannelKey = \"__goToolNameMap\"",
		"func needsThirdPartyCloak(",
		"func isAnthropicServerToolType(",
		"func toPascalCaseToolName(",
		"func remapToolNamesInRequest(",
		"func remapToolNamesInResponse(",
		"func restoreClaudeToolName(",
		"func cloakThirdPartyToolNames(",
		"func detachToolNameMap(",
		"func takeToolNameMap(",
		"func caseInsensitiveToolNameLookup(",
		"func restoreOpenAIToolNames(",
		"var toolRenameMap = map[string]string{",
		"var toolReverseMap = map[string]string{",
		"var harnessCanonicalMap = map[string]string{",
		"var claudeBuiltinToolNames",
		"versionedServerToolType",
		"nonVersionedServerToolTypes",
	} {
		if !strings.Contains(src, token) {
			t.Errorf("claude_tool_remap.go 缺少 %q", token)
		}
	}

	// extra remap 的两条必须原样在表里（claudeCodeExtraRemap.ts:1-18）。
	if !strings.Contains(src, `"subagents":`) || !strings.Contains(src, `"SubDispatch"`) {
		t.Errorf("toolRenameMap 缺少 subagents -> SubDispatch")
	}
	if !strings.Contains(src, `"session_status":`) || !strings.Contains(src, `"CheckStatus"`) {
		t.Errorf("toolRenameMap 缺少 session_status -> CheckStatus")
	}
}

// TestToolRemap接线_chain分支透传映射 锁候选链两个形状都拿到映射。
func TestToolRemap接线_chain分支透传映射(t *testing.T) {
	src := readAppSrc(t, "routing_dispatch.go")

	// 流式: responseToolNameMap 必须从 hopParams 取（同一对象引用）。
	if got := countToken(src, "takeToolNameMap(hopParams)"); got < 1 {
		t.Fatalf("takeToolNameMap(hopParams) 未出现 —— 流式/非流式链路的映射来源缺失")
	}
	// 流式 Claude 分支必须传 responseToolNameMap。
	if got := countToken(src, "handleAnthropicStreamWithToolNameMap(w, resp, model, tgt.ToolSchemas, observe, responseToolNameMap)"); got != 1 {
		t.Fatalf("流式 Claude 分支未透传映射, 出现 %d 次, 期望 1", got)
	}
	// 非流式 OpenAI 分支必须传 toolNameMap。
	if got := countToken(src, "handleNonStreamResponseWithToolNameMap(w, resp, observeUsage, toolNameMap)"); got != 1 {
		t.Fatalf("非流式 OpenAI 分支未透传映射, 出现 %d 次, 期望 1", got)
	}
	// 非流式 Claude 分支必须传 toolNameMap。
	if got := countToken(src, "openAIToAnthropicWithMap(chatOut, toolNameMap)"); got != 1 {
		t.Fatalf("非流式 Claude 分支未透传映射, 出现 %d 次, 期望 1", got)
	}
}
