package app

import "strings"

// 逐字照抄 OmniRoute open-sse/handlers/chatCore/claudeSystemRole.ts (247 行)。
//
// 参考实现原注释 (claudeSystemRole.ts:1-13):
//
//	chatCore Claude system-role lifter (Quality Gate v2 / Fase 9 — chatCore god-file
//	decomposition, #3501).
//
//	Pure helper extracted from chatCore.ts: lifts any `system`/`developer` role messages out of the
//	messages[] array into the top-level `system` field. Anthropic's Messages API rejects either as a
//	chat role, so they must be hoisted. `developer` is OpenAI's Responses-API rename of `system` and
//	is treated identically. Mutates the payload in place; behaviour is byte-identical to the previous
//	top-level definition (still re-exported from chatCore.ts for existing importers/tests).
//
//	`relocateHoistedCacheBoundary` keeps that hoist from destroying the client's prompt-cache
//	layout (#9436); both hoisting implementations share it.
//
// # 调用作用域 (已核实, 见 chatCore.ts:2305-2320)
//
//	参考实现只在「Claude 语义透传」这条路径上调用本模块:
//
//	  if (isClaudeCodeSemanticPassthrough) {
//	    if (provider !== "claude" || !shouldUseMidConversationSystem(...)) {
//	      extractSystemRoleMessages(translatedBody);   // ← 主路径
//	    } else {
//	      relocateDirectiveOnlyMessages(translatedBody); // ← 1M-context Opus 例外
//	    }
//	    ...
//	  }
//
//	其中的 mid-conversation-system 分支 (claudeIdentity.ts:326-341) 仅在
//	provider === "claude" 且模型命中 `claude-opus` 且同时带 system + tools 时成立 ——
//	即 Anthropic 1M beta 档位。**其余所有路径都走 extractSystemRoleMessages。**
//
// # Go 侧语义对齐要点
//
//   - **就位改写 (in-place)**: 参考实现直接改 payload 的 messages/system/output_config 键,
//     不返回新对象。Go 侧用 (messages, system, outputConfig) 三元组进出, 由调用方回写,
//     语义等价 (参考实现体内 `payload` 的其它键从不被触碰)。
//   - **返回 nil 表示"未改写"**: 对应参考实现的早退分支 (messages 非数组 / 无 system 角色)。
//     hoistedBlock 与 system 需区分「键不存在」与「值为 nil」, 故额外用布尔位。
//   - **`in` 判定用 JS 语义**: `payload.system == null` 在 JS 里同时覆盖 undefined 与 null,
//     而 `typeof existingSystem === "string"` 只认字符串 —— 我方用 hasSystem 布尔位 + 类型断言。
//   - **cache_control 是对象引用搬运, 不是深拷贝**: 参考实现把 marker 这个**同一个对象**
//     挂到目标块上, 并在 caller 处 `delete hoisted.cache_control`。Go 侧同样只搬引用。
//   - **映射迭代顺序**: 本模块不依赖 Go map 的迭代顺序 (只做键存在性判断与逐键拷贝)。

// hoistedCacheBoundary 照抄 claudeSystemRole.ts:15 的
// `export type HoistedCacheBoundary = "moved" | "kept" | "dropped";`
type hoistedCacheBoundary string

const (
	// hoistedCacheBoundaryMoved: 调用方必须把 marker 从被提升的块上移除。
	hoistedCacheBoundaryMoved hoistedCacheBoundary = "moved"
	// hoistedCacheBoundaryKept: marker 留在被提升的块上。
	hoistedCacheBoundaryKept hoistedCacheBoundary = "kept"
	// hoistedCacheBoundaryDropped: 调用方必须把 marker 从被提升的块上移除 (TTL 顺序不允许)。
	hoistedCacheBoundaryDropped hoistedCacheBoundary = "dropped"
)

// effectiveTtl 照抄 claudeSystemRole.ts:17-21。
//
// `Effective cache TTL of a cache_control value; Anthropic defaults to 5m when ttl is absent.`
func effectiveTtl(marker any) string {
	// :19 `const ttl = (marker as ...)?.ttl;`
	// :20 `return typeof ttl === "string" ? ttl : "5m";`
	mm, ok := marker.(map[string]any)
	if !ok {
		return "5m"
	}
	if ttl, ok := mm["ttl"].(string); ok {
		return ttl
	}
	return "5m"
}

// jsPartText 复刻 `(part as {text?: unknown})?.text` 在 `typeof text === "string"` 下取值。
// 非对象 / 无该键 / 非字符串 → ("", false)。
func jsPartText(part any) (string, bool) {
	pm, ok := part.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := pm["text"].(string)
	return s, ok
}

// isCacheBreakpointTarget 照抄 claudeSystemRole.ts:27-62。
//
// `Whether a content block can carry a cache breakpoint. Excludes blocks Anthropic does not accept
// as one (thinking) and blocks the upstream normalisation discards or empties out anyway.`
func isCacheBreakpointTarget(block any) bool {
	// :28 `if (block === null || typeof block !== "object") return false;`
	if block == nil {
		return false
	}
	candidate, ok := block.(map[string]any)
	if !ok {
		return false
	}
	// :30 `switch (candidate.type) {`
	typeName, _ := candidate["type"].(string)
	switch typeName {
	case "text":
		// :32 `// Empty text blocks are stripped before the payload goes upstream.`
		// :33 `return typeof candidate.text === "string" && candidate.text.length > 0;`
		text, isStr := candidate["text"].(string)
		return isStr && len(text) > 0
	case "tool_use", "image", "image_url", "file", "file_url", "document":
		// :34-40 `case "tool_use": case "image": ... return true;`
		return true
	case "tool_result":
		// :42 `// A tool_result that yields no text collapses to nothing during normalisation.`
		// :43 `const payload = candidate.content ?? candidate.text ?? candidate.output;`
		payload, hasPayload := jsCoalesce(candidate, "content", "text", "output")
		// :44 `if (typeof payload === "string") return payload.length > 0;`
		if s, ok := payload.(string); ok {
			return len(s) > 0
		}
		// :45 `if (Array.isArray(payload)) {`
		if arr, ok := payload.([]any); ok {
			// :46 `// Only the non-empty text parts of the array survive; images and unknown parts do not.`
			// :47-54 `return payload.some((part) => part?.type === "text" && typeof part.text === "string"
			//          && part.text.length > 0);`
			for _, part := range arr {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := pm["type"].(string); t != "text" {
					continue
				}
				if s, ok := pm["text"].(string); ok && len(s) > 0 {
					return true
				}
			}
			return false
		}
		// :56 `return payload != null;`
		// JS 的 `!= null` 同时排除 undefined 与 null。
		return hasPayload && payload != nil
	default:
		// :58-60 `default: // thinking, redacted_thinking, and anything unrecognised. return false;`
		return false
	}
}

// jsCoalesce 复刻 JS 的 `a ?? b ?? c`:
// 返回首个「既存在且非 null/undefined」的值。全为空时返回 (nil, false)。
func jsCoalesce(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// relocateHoistedCacheBoundary 照抄 claudeSystemRole.ts:64-98。
//
// `Preserves a message-level cache boundary when a marked system/developer block is hoisted into
// top-level system[].`
//
// `The marker is moved to the nearest preceding block that can carry a breakpoint. If that block is
// already marked, both are kept — except where the hoisted marker, which ends up ahead of the
// target in system[], would put a 5m breakpoint before a 1h one; Anthropic requires the longer
// TTL first, so the hoisted marker is dropped instead.`
//
// `@returns "moved" or "dropped" — the caller must remove the marker from the hoisted block;
//
//	"kept" — the marker stays on it`
//
// preceding 的元素形态对应参考实现的 `{ content?: unknown }`。Go 侧传 []any, 元素为
// map[string]any; 非 map 元素按「无 content」处理 (与参考实现对 null/undefined 的行为一致)。
func relocateHoistedCacheBoundary(marker any, preceding []any) hoistedCacheBoundary {
	// :80 `for (let i = preceding.length - 1; i >= 0; i--) {`
	for i := len(preceding) - 1; i >= 0; i-- {
		// :81 `const content = preceding[i]?.content;`
		var content any
		if pm, ok := preceding[i].(map[string]any); ok {
			content = pm["content"]
		}
		// :82 `if (!Array.isArray(content)) continue;`
		blocks, ok := content.([]any)
		if !ok {
			continue
		}
		// :83 `for (let j = content.length - 1; j >= 0; j--) {`
		for j := len(blocks) - 1; j >= 0; j-- {
			block := blocks[j]
			// :85 `if (!isCacheBreakpointTarget(block)) continue;`
			if !isCacheBreakpointTarget(block) {
				continue
			}
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			// :86 `if (block.cache_control == null) {`
			if cc, exists := bm["cache_control"]; !exists || cc == nil {
				// :87 `block.cache_control = marker;`
				bm["cache_control"] = marker
				// :88 `return "moved";`
				return hoistedCacheBoundaryMoved
			}
			// :90-91 `// Occupied: overwriting would discard the client's own marker, and stepping
			//          further back would only shorten the prefix — so both stay, unless the TTL
			//          order forbids it.`
			// :92-94 `return effectiveTtl(marker) === "5m" && effectiveTtl(block.cache_control) === "1h"
			//            ? "dropped" : "kept";`
			if effectiveTtl(marker) == "5m" && effectiveTtl(bm["cache_control"]) == "1h" {
				return hoistedCacheBoundaryDropped
			}
			return hoistedCacheBoundaryKept
		}
	}
	// :97 `return "kept";`
	return hoistedCacheBoundaryKept
}

// isSystemRoleJS 照抄 claudeSystemRole.ts:107-109 / :184-186 的 `isSystemRole` 闭包 (两处逐字相同)。
//
// `typeof role === "string" && (role.toLowerCase() === "system" || role.toLowerCase() === "developer")`
func isSystemRoleJS(role any) bool {
	s, ok := role.(string)
	if !ok {
		return false
	}
	lower := strings.ToLower(s)
	return lower == "system" || lower == "developer"
}

// extractSystemRoleMessages 照抄 claudeSystemRole.ts:100-165。
//
// 参数与返回值:
//   - messages: payload["messages"] 断言后的切片 (调用方保证是数组, 否则本函数早退)。
//   - system:   payload["system"] 的当前值; hasSystem 标记该键是否存在
//     (JS 里 `payload.system` 读不存在的键得 undefined, 与 null 一同被 `== null` 捕获)。
//   - outputConfig: payload["output_config"] 的当前值; hasOutputConfig 同理。
//
// 返回 (newMessages, newSystem, newOutputConfig, changed):
//   - changed=false 时调用方不应回写任何键 (对应参考实现的早退)。
//   - newSystem 为 nil 且 systemHadValue=false 时表示「不设该键」。
func extractSystemRoleMessages(
	messages []any,
	system any, hasSystem bool,
	outputConfig any, hasOutputConfig bool,
) ([]any, any, any, bool) {
	// :101 `if (!Array.isArray(payload.messages)) return;`
	// —— 调用方已断言为切片, 此处即「数组存在但为空」仍继续 (参考实现继续)。
	var systemMessages int
	for _, m := range messages {
		if mm, ok := m.(map[string]any); ok && isSystemRoleJS(mm["role"]) {
			systemMessages++
		}
	}
	// :111 `if (systemMessages.length === 0) return;`
	if systemMessages == 0 {
		return messages, system, outputConfig, false
	}

	// :113 `const extraBlocks: Array<Record<string, unknown>> = [];`
	extraBlocks := make([]any, 0, systemMessages)
	// :114-115 `// Walk in order rather than over the filtered list: re-anchoring a hoisted
	//           cache_control needs the messages that precede it and stay behind (#9436).`
	// :116 `const preceding: Array<{ content?: unknown }> = [];`
	preceding := make([]any, 0, len(messages))
	outOutputConfig := outputConfig
	outHasOutputConfig := hasOutputConfig

	// :117 `for (const sm of messages) {`
	for _, sm := range messages {
		smMap, isMap := sm.(map[string]any)
		// :118 `if (!isSystemRole(sm.role)) { preceding.push(sm); continue; }`
		// 参考实现对非对象元素读 `.role` 得 undefined → !isSystemRole → 进 preceding。
		if !isMap || !isSystemRoleJS(smMap["role"]) {
			preceding = append(preceding, sm)
			continue
		}
		// :122 `if (typeof sm.content === "string" && sm.content.length > 0) {`
		switch content := smMap["content"].(type) {
		case string:
			if len(content) > 0 {
				// :123 `extraBlocks.push({ type: "text", text: sm.content });`
				extraBlocks = append(extraBlocks, map[string]any{"type": "text", "text": content})
			}
		case []any:
			// :124 `} else if (Array.isArray(sm.content)) {`
			// :125 `for (const block of sm.content as Array<Record<string, unknown>>) {`
			for _, block := range content {
				bm, ok := block.(map[string]any)
				if !ok {
					continue
				}
				// :126 `if (block?.type === "text" && typeof block.text === "string"
				//        && block.text.length > 0) {`
				if t, _ := bm["type"].(string); t != "text" {
					continue
				}
				text, _ := bm["text"].(string)
				if len(text) == 0 {
					continue
				}
				// :127 `const hoisted = { ...block };` —— 浅拷贝
				hoisted := make(map[string]any, len(bm))
				for k, v := range bm {
					hoisted[k] = v
				}
				// :128-133
				// `if (hoisted.cache_control != null &&
				//    relocateHoistedCacheBoundary(hoisted.cache_control, preceding) !== "kept") {
				//    delete hoisted.cache_control;
				//  }`
				if cc, exists := hoisted["cache_control"]; exists && cc != nil {
					if relocateHoistedCacheBoundary(cc, preceding) != hoistedCacheBoundaryKept {
						delete(hoisted, "cache_control")
					}
				}
				// :134 `extraBlocks.push(hoisted);`
				extraBlocks = append(extraBlocks, hoisted)
			}
		}
		// :138-142 `// Directive payload (message-level output_config, as emitted by Claude
		//           Code clients): the message itself is lifted away, so fold its output
		//           configuration into the top-level parameter instead of silently dropping
		//           it — whatever shape the content had. An explicit top-level output_config
		//           wins, and among several directive messages the first one wins.`
		// :143 `if (payload.output_config == null) {`
		if !outHasOutputConfig || outOutputConfig == nil {
			// :144 `const directive = sm as Record<string, unknown>;`
			// :145-149 `if (directive.output_config != null &&
			//            typeof directive.output_config === "object" &&
			//            !Array.isArray(directive.output_config)) {`
			if directive, exists := smMap["output_config"]; exists && directive != nil {
				_, isArr := directive.([]any)
				if !isArr {
					switch directive.(type) {
					case map[string]any:
						// :150 `payload.output_config = directive.output_config;`
						outOutputConfig = directive
						outHasOutputConfig = true
					}
				}
			}
		}
	}

	// :154 `if (extraBlocks.length > 0) {`
	if len(extraBlocks) > 0 {
		// :155 `const existingSystem = payload.system;`
		// :156 `if (typeof existingSystem === "string" && existingSystem.length > 0) {`
		if s, ok := system.(string); ok && len(s) > 0 {
			// :157 `payload.system = [{ type: "text", text: existingSystem }, ...extraBlocks];`
			merged := make([]any, 0, len(extraBlocks)+1)
			merged = append(merged, map[string]any{"type": "text", "text": s})
			merged = append(merged, extraBlocks...)
			system = merged
			hasSystem = true
		} else if existing, ok := system.([]any); ok {
			// :158 `} else if (Array.isArray(existingSystem)) {`
			// :159 `payload.system = [...existingSystem, ...extraBlocks];`
			merged := make([]any, 0, len(existing)+len(extraBlocks))
			merged = append(merged, existing...)
			merged = append(merged, extraBlocks...)
			system = merged
			hasSystem = true
		} else {
			// :160-162 `} else { payload.system = extraBlocks; }`
			system = extraBlocks
			hasSystem = true
		}
	}
	// :164 `payload.messages = messages.filter((m) => !isSystemRole(m.role));`
	filtered := make([]any, 0, len(messages)-systemMessages)
	for _, m := range messages {
		if mm, ok := m.(map[string]any); ok && isSystemRoleJS(mm["role"]) {
			continue
		}
		// 参考实现 `messages.filter((m) => !isSystemRole(m.role))`:
		// 非对象元素的 `.role` 是 undefined → !isSystemRole → **保留**。
		filtered = append(filtered, m)
	}
	return filtered, system, outOutputConfig, true
}

// relocateDirectiveOnlyMessages 照抄 claudeSystemRole.ts:167-247。
//
// `Moves a directive-only system message (empty content array + message-level output_config,
// the shape Claude Code clients emit) off messages[0].`
//
// `Anthropic treats messages[0] as the initial system prompt position and rejects the directive-only
// form there ("use the top-level 'system' parameter for the initial system prompt"), while accepting
// it at any other position. The mid-conversation-system passthrough (provider claude + 1M-context
// beta models) deliberately keeps system-role messages inside messages[], so a directive that
// arrived first would go upstream unchanged and 400. Relocate it past the first real turn instead;
// when the conversation has no real turn at all, fold the output_config into the top-level parameter
// (which wins when already present) and drop the now-empty message.`
//
// 返回 (newMessages, newOutputConfig, changed)。
func relocateDirectiveOnlyMessages(
	messages []any,
	outputConfig any, hasOutputConfig bool,
) ([]any, any, bool) {
	// :182 `if (!Array.isArray(payload.messages) || payload.messages.length === 0) return;`
	if len(messages) == 0 {
		return messages, outputConfig, false
	}

	// :187-192 `const isEmptySystem = (m) => m != null && typeof m === "object"
	//            && isSystemRole(m.role) && Array.isArray(m.content) && m.content.length === 0;`
	isEmptySystem := func(m any) bool {
		mm, ok := m.(map[string]any)
		if !ok {
			return false
		}
		if !isSystemRoleJS(mm["role"]) {
			return false
		}
		content, ok := mm["content"].([]any)
		return ok && len(content) == 0
	}
	// :193-197 `const isDirectiveOnly = (m) => isEmptySystem(m) && m.output_config != null
	//            && typeof m.output_config === "object" && !Array.isArray(m.output_config);`
	isDirectiveOnly := func(m any) bool {
		if !isEmptySystem(m) {
			return false
		}
		mm, _ := m.(map[string]any)
		oc, exists := mm["output_config"]
		if !exists || oc == nil {
			return false
		}
		if _, isArr := oc.([]any); isArr {
			return false
		}
		_, isMap := oc.(map[string]any)
		return isMap
	}

	// :199 `if (!isEmptySystem(messages[0])) { return; }`
	if !isEmptySystem(messages[0]) {
		return messages, outputConfig, false
	}

	// :203-205 `// Collect the whole leading run of empty system messages so consecutive
	//            directives are all relocated in one pass (handling only messages[0] would
	//            leave the second directive at the rejected position).`
	// :206-209
	runEnd := 0
	for runEnd < len(messages) && isEmptySystem(messages[runEnd]) {
		runEnd++
	}
	// :210 `const lead = messages.slice(0, runEnd);`
	lead := messages[:runEnd]
	// :211 `const directives = lead.filter(isDirectiveOnly);`
	directives := make([]any, 0, len(lead))
	for _, m := range lead {
		if isDirectiveOnly(m) {
			directives = append(directives, m)
		}
	}

	// :213-215 `// First real (user/assistant) turn after the run. System messages with text
	//            content are not safe insertion anchors — keep walking past them, and past
	//            any non-object entries a malformed body may carry.`
	// :216 `let insertAfter = -1;`
	insertAfter := -1
	// :217-227
	for i := runEnd; i < len(messages); i++ {
		candidate := messages[i]
		cm, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if !isSystemRoleJS(cm["role"]) {
			insertAfter = i
			break
		}
	}

	// :229 `if (insertAfter === -1) {`
	if insertAfter == -1 {
		// :230-232 `// No real turn to relocate after: fold the first directive's
		//            output_config into the top-level parameter (an explicit top-level value
		//            wins) and drop the whole run.`
		// :233 `if (payload.output_config == null && directives.length > 0) {`
		if (!hasOutputConfig || outputConfig == nil) && len(directives) > 0 {
			// :234 `payload.output_config = directives[0].output_config;`
			dm, _ := directives[0].(map[string]any)
			outputConfig = dm["output_config"]
			hasOutputConfig = true
		}
		// :236 `payload.messages = messages.slice(runEnd);`
		return messages[runEnd:], outputConfig, true
	}

	// :240-241 `// Move the directives (in order) past the first real turn; plain empty
	//            system messages carry nothing and are dropped.`
	// :242-246
	// `payload.messages = [...messages.slice(runEnd, insertAfter + 1), ...directives,
	//                       ...messages.slice(insertAfter + 1)];`
	tail := messages[insertAfter+1:]
	out := make([]any, 0, (insertAfter+1-runEnd)+len(directives)+len(tail))
	out = append(out, messages[runEnd:insertAfter+1]...)
	out = append(out, directives...)
	out = append(out, tail...)
	return out, outputConfig, true
}

// providerSupportsMidConversationSystem 照抄 executors/claudeIdentity.ts:326-341
// 的 `shouldUseMidConversationSystem` —— 决定是否保留 messages[] 内的 system 角色
// (Anthropic 1M-context beta 档位)。
//
// 参考实现原注释 (claudeIdentity.ts:300-308):
//
//	Models that support the context-1m beta tier. Only Opus is eligible;
//	Sonnet trips long-context credit gates under OAuth full-agent traffic.
//	Opus 5 is excluded because its 1M context window is native.
//
//	`return hasSystem && hasTools && matchesModelPrefix(effectiveModel, CONTEXT_1M_BETA_MODEL_PREFIXES)`
//
// 入参 hasSystem 对应 `!!payload.system && (typeof payload.system === "string" ||
// (Array.isArray(payload.system) && payload.system.length > 0))` 的判定结果,
// hasTools 对应 `Array.isArray(payload.tools) && payload.tools.length > 0`。
func providerSupportsMidConversationSystem(hasSystem bool, hasTools bool, model string) bool {
	return hasSystem && hasTools && claudeMatchesModelPrefix(model, claudeContext1mBetaModelPrefixes)
}

// claudeContext1mBetaModelPrefixes 照抄 claudeIdentity.ts:305 的
// `const CONTEXT_1M_BETA_MODEL_PREFIXES = ["claude-opus"];`
var claudeContext1mBetaModelPrefixes = []string{"claude-opus"}

// claudeContext1mNativeModelPrefixes 照抄 claudeIdentity.ts:306 的
// `const CONTEXT_1M_NATIVE_MODEL_PREFIXES = ["claude-opus-5"];`
var claudeContext1mNativeModelPrefixes = []string{"claude-opus-5"}

// claudeMatchesModelPrefix 照抄 claudeIdentity.ts:308-312。
//
//	`function matchesModelPrefix(model: unknown, prefixes: string[]): boolean {
//	   if (typeof model !== "string") return false;
//	   const normalized = model.toLowerCase();
//	   return prefixes.some((prefix) => normalized.includes(prefix));
//	 }`
func claudeMatchesModelPrefix(model string, prefixes []string) bool {
	normalized := strings.ToLower(model)
	for _, prefix := range prefixes {
		if strings.Contains(normalized, prefix) {
			return true
		}
	}
	return false
}
