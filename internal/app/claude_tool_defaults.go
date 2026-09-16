package app

// 逐字照抄 OmniRoute open-sse/handlers/chatCore/claudeToolDefaults.ts (27 行)。
//
// 原注释 :5-18:
//
//	Claude's tool schema requires every tool to carry an explicit `type` discriminator
//	(e.g. "custom", "computer_20241022", "bash_20241022"). Anthropic's own API infers
//	"custom" when it's omitted, but strict Anthropic-compatible gateways (e.g. MiniMax)
//	enforce the documented schema and reject payloads whose tools lack `type` with
//	HTTP 400. Default a missing `type` to "custom" so legacy Claude-format tool
//	definitions survive strict gateways, while leaving any tool that already declares a
//	type (incl. built-in tool types) untouched. (port from 9router#2195)
//
//	Non-array input is returned unchanged; defaulted entries are new objects so the
//	caller's original tool objects are not mutated. Non-object array entries (null,
//	primitives, arrays) are passed through untouched rather than wrapped — spreading
//	a primitive would fabricate a garbage tool (e.g. `{ type: "custom", '0': 'h' }`).
//
// # Go 侧语义对齐要点
//
//   - **不修改入参**: 参考实现用 `{ type: "custom", ...tool }` 造**新对象**，
//     JS 展开语义是"新键在前、旧键在后"→ 若 tool 自带 type 本分支就不会走到，
//     所以新旧键不存在覆盖冲突。Go 侧同样构造新 map。
//   - **非对象条目原样透传**: null / 基本类型 / 数组都不包装。这条是原注释
//     显式点名的反直觉点("spreading a primitive would fabricate a garbage tool")。
//     Go 侧 `[]any` 里可能出现任何类型，故必须逐个做类型断言。
//   - **非数组输入原样返回**: 调用方可能传 nil / 字符串，不能 panic。
func defaultClaudeToolType(tools any) any {
	// :20 `if (!Array.isArray(tools)) return tools;`
	arr, ok := tools.([]any)
	if !ok {
		return tools
	}
	// :21 `return tools.map((tool) => {...})`
	out := make([]any, 0, len(arr))
	for _, tool := range arr {
		// :22 `if (tool && typeof tool === "object" && !Array.isArray(tool))`
		rec, isObj := tool.(map[string]any)
		if isObj {
			// :23 `return (tool as UnknownRecord).type ? tool : { type: "custom", ...tool };`
			//
			// JS 的 `tool.type` 是"真值"判定: 空串 / 0 / null / undefined 都算 falsy。
			// Go 侧等价写法: 断言成 string 且非空。
			if t, ok := rec["type"].(string); ok && t != "" {
				out = append(out, tool)
			} else {
				merged := map[string]any{"type": "custom"}
				for k, v := range rec {
					merged[k] = v
				}
				out = append(out, merged)
			}
			continue
		}
		// :25 `return tool;` —— 非对象条目原样透传
		out = append(out, tool)
	}
	return out
}
