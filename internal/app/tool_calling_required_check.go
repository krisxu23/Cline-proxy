package app

import "fmt"

// 逐字照抄 OmniRoute open-sse/handlers/chatCore/toolCallingRequiredCheck.ts (33 行)。
//
// 原注释 :1-12:
//
//	Extracted from handleChatCore (chatCore god-file decomposition).
//
//	Combo requests are protected from ever reaching a tool-incapable target in
//	the first place by filterTargetsByRequestCompatibility (comboStructure.ts,
//	backed by getResolvedModelCapabilities' toolCalling resolution). But a
//	direct/pinned request (isCombo: false) has no other target to fail over
//	to — silently stripping `tools` and flattening history there produces a
//	200 response that can never actually call anything (the model narrates a
//	fake action instead, live incident: AI Horde/Behemoth-X-123B). A clear,
//	explicit error is better than a response that looks successful but
//	silently drops the client's intended action.
//
// 这条正是用户报的"任务无声中断 / 模型只是叙述了一个假动作"那类故障的**防护栏**:
// 当目标模型不支持工具调用、而我们又把 tools 剥掉并扁平化历史时, 会返回一个
// HTTP 200 但永远无法真正调用任何工具的响应 —— 模型改用散文**假装**执行了操作。
// 对组合/回退请求 (isCombo=true) 无所谓, 因为还有别的目标可换; 但**直连/固定**
// 请求没有备选, 此时必须显式报错, 而不是给一个"看起来成功"的响应。
//
// # Go 侧语义对齐要点
//
// 三条短路条件必须逐条对应 (`:25-27`), **顺序不可调换也无需调换**(三条互不依赖):
//  1. `if (isCombo) return { blocked: false };`
//  2. `if (!unsupported.includes("tools")) return { blocked: false };`
//  3. `if (!Array.isArray(body.tools) || body.tools.length === 0) return { blocked: false };`
//
// 注意第 3 条的 `!Array.isArray(body.tools)` 分支: 我方 Go 侧 body["tools"] 是 `any`,
// 既可能不存在(键缺失), 也可能是非数组(畸形客户端)。**两者都返回不拦截**,
// 与参考实现一致 —— 只有"确实是非空数组"才拦。
type toolCallingCheckResult struct {
	blocked bool
	message string
}

// checkToolCallingRequiredButUnsupported 照抄 toolCallingRequiredCheck.ts:19-33。
func checkToolCallingRequiredButUnsupported(body map[string]any, unsupported []string, isCombo bool, model string) toolCallingCheckResult {
	// :25 `if (isCombo) return { blocked: false };`
	if isCombo {
		return toolCallingCheckResult{blocked: false}
	}
	// :26 `if (!unsupported.includes("tools")) return { blocked: false };`
	if !stringSliceContains(unsupported, "tools") {
		return toolCallingCheckResult{blocked: false}
	}
	// :27 `if (!Array.isArray(body.tools) || body.tools.length === 0) return { blocked: false };`
	toolsRaw, exists := body["tools"]
	if !exists {
		return toolCallingCheckResult{blocked: false}
	}
	toolsArr, isArr := toolsRaw.([]any)
	if !isArr || len(toolsArr) == 0 {
		return toolCallingCheckResult{blocked: false}
	}

	// :29-32 return { blocked: true, message: `Model "${model}" does not support
	//        tool calling. Remove "tools" from the request or choose a different model.` };
	return toolCallingCheckResult{
		blocked: true,
		message: fmt.Sprintf(
			`Model "%s" does not support tool calling. Remove "tools" from the request or choose a different model.`,
			model,
		),
	}
}

// stringSliceContains 等价于 JS 的 `arr.includes(x)`。
func stringSliceContains(arr []string, target string) bool {
	for _, s := range arr {
		if s == target {
			return true
		}
	}
	return false
}
