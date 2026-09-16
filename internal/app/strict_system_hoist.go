package app

import (
	"os"
	"strings"
)

// 逐字照抄 OmniRoute open-sse/translator/helpers/strictSystemHoist.ts (66 行)
// + src/lib/memory/injection.ts:84 / :90-118（严格 provider 清单与判定）。
//
// 参考实现原注释 (strictSystemHoist.ts:27-35):
//
//	#7293: hoist every `system`-role message onto index 0 for providers that reject a
//	non-first system message (`systemMessageMustBeFirst()` — the single source of truth
//	already used by `src/lib/memory/injection.ts`'s memory-injection half, #6135/PR#6225).
//
//	`translateRequest()` is the single outbound choke point every request passes through,
//	including same-format (OpenAI→OpenAI) passthrough where none of the format-specific
//	translators run — so a client-injected `system` message landing mid-array (OpenCode /
//	Kilo Code style clients, Discussion #6129) previously reached the upstream untouched.
//
//	Merge, never drop: multiple offending system messages are folded (in original order)
//	into the single leading system message, mirroring `injectSystemFirst()`'s
//	`${memoryText}\n${first.content}` pattern and `openai-to-claude.ts`'s system-array-merge
//	pattern.
//
//	No-op (same array reference) whenever the provider is not strict, or the request is
//	already compliant — required for prompt-cache prefix stability (#3890 class).
//
// # 与用户场景的关系
//
// 用户的 agent 客户端（Codex / OpenCode / Kilo Code 风格）会把 `system` 消息插在
// 数组中间。对严格 provider（参考实现清单含 `xiaomi-mimo` / `mimo` / `tokenrouter`）
// 这类请求会被上游直接拒绝, 表现为任务无声中断。
//
// # Go 侧语义对齐要点
//
//   - **No-op 必须"返回原切片"而非"返回等值新切片"**: 原注释点名这是
//     prompt-cache 前缀稳定性的要求(#3890)。调用方可能用切片指针比较判断是否变动,
//     故不合规时也**不重建切片**。
//   - **Merge 不 drop**: 多个 system 消息按原顺序折进首个 system。注意首个若本身
//     是 system, 它的文本**排在前面**(`rest[0]` 先, 然后才是 offending)。
//   - **空文本被过滤**: `toTextContent` 返回空串的条目不参与拼接(`.filter(Boolean)`)。
//   - **最终拼接用 `\n`**(不是空串) —— 与 flattenToolHistory 的 `extractTextContent`
//     (空串连接)不同, 两处不可混用。

// builtinProvidersSystemMustBeFirst 照抄 injection.ts:84 的内置清单。
// `tokenrouter` 正是用户使用的 provider 之一, 故这条规则在本网关会真实触发。
var builtinProvidersSystemMustBeFirst = map[string]bool{
	"xiaomi-mimo": true,
	"mimo":        true,
	"tokenrouter": true,
}

// 我方沿用参考实现的同名环境变量, 保持可迁移性(取值: 逗号分隔 provider id)。
const strictSystemProvidersEnv = "OMNIROUTE_STRICT_SYSTEM_PROVIDERS"

// getStrictSystemProvidersEnv 读取扩展清单的原始值。
func getStrictSystemProvidersEnv() string {
	return os.Getenv(strictSystemProvidersEnv)
}

// parseStrictSystemProvidersEnv 复刻参考实现的 `OMNIROUTE_STRICT_SYSTEM_PROVIDERS`
// 环境变量扩展口 (injection.ts:90-98)：逗号分隔、小写、去空白、滤空串。
func parseStrictSystemProvidersEnv(raw string) []string {
	// :94-97 `raw.split(",").map(id => id.trim().toLowerCase()).filter(Boolean)`
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, id := range parts {
		id = strings.ToLower(strings.TrimSpace(id))
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// systemMessageMustBeFirst 照抄 injection.ts:111-118。
func systemMessageMustBeFirst(provider string, extraProviderIDs []string) bool {
	// :115 `if (!provider) return false;`
	if provider == "" {
		return false
	}
	// :116 `const normalized = provider.toLowerCase().trim();`
	normalized := strings.ToLower(strings.TrimSpace(provider))
	// :117 `return resolveProvidersSystemMustBeFirst(env).has(normalized);`
	if builtinProvidersSystemMustBeFirst[normalized] {
		return true
	}
	for _, id := range extraProviderIDs {
		if id == normalized {
			return true
		}
	}
	return false
}

// strictSystemHoistToTextContent 照抄 strictSystemHoist.ts:6-17 的 `toTextContent`。
//
//   - 字符串 → 原样
//   - 数组 → 只保留 `type === "text"` 的块, 取 `part.text ?? ""` 转字符串,
//     用 **`\n`** 连接
//   - 其余 → 空串
func strictSystemHoistToTextContent(content any) string {
	// :7 `if (typeof content === "string") return content;`
	if s, ok := content.(string); ok {
		return s
	}
	// :8-16 数组分支
	arr, ok := content.([]any)
	if !ok {
		// :17 `return "";`
		return ""
	}
	texts := make([]string, 0, len(arr))
	for _, part := range arr {
		// `.filter((part) => Boolean(part) && typeof part === "object"
		//            && (part as {type?: unknown}).type === "text")`
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := pm["type"].(string); !ok || t != "text" {
			continue
		}
		// `.map((part) => String(part.text ?? ""))`
		texts = append(texts, jsStringifyOrEmpty(pm["text"]))
	}
	// `.join("\n")`
	return strings.Join(texts, "\n")
}

// hoistLeadingSystemMessage 照抄 strictSystemHoist.ts:36-65。
//
// 返回的切片在"非严格 provider"或"本就合规"时**与入参同一底层数组**(no-op),
// 见上方注释的 prompt-cache 要求。
func hoistLeadingSystemMessage(messages []any, provider string, extraProviderIDs []string) []any {
	// :40 `if (!Array.isArray(messages) || messages.length === 0) return messages;`
	if len(messages) == 0 {
		return messages
	}
	// :41 `if (!systemMessageMustBeFirst(provider)) return messages;`
	if !systemMessageMustBeFirst(provider, extraProviderIDs) {
		return messages
	}

	// :43-46 收集 index > 0 的 system 消息下标
	offendingIndices := make([]int, 0, len(messages))
	for i := 1; i < len(messages); i++ {
		mm, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" {
			offendingIndices = append(offendingIndices, i)
		}
	}
	// :47 `if (offendingIndices.length === 0) return messages;`
	if len(offendingIndices) == 0 {
		return messages
	}

	// :49 `const offending = offendingIndices.map((i) => messages[i]);`
	// :50 `const rest = messages.filter((_, i) => !offendingIndices.includes(i));`
	offendingSet := make(map[int]bool, len(offendingIndices))
	for _, i := range offendingIndices {
		offendingSet[i] = true
	}
	rest := make([]any, 0, len(messages)-len(offendingIndices))
	for i, m := range messages {
		if !offendingSet[i] {
			rest = append(rest, m)
		}
	}

	// :52-56 拼接文本: 若 rest[0] 是 system, 它的文本**排最前**, 然后是 offending。
	// `.filter((text): text is string => Boolean(text))` —— 空串被过滤。
	texts := make([]string, 0, len(offendingIndices)+1)
	var firstRest map[string]any
	firstRestIsSystem := false
	if len(rest) > 0 {
		if fm, ok := rest[0].(map[string]any); ok {
			if role, _ := fm["role"].(string); role == "system" {
				firstRestIsSystem = true
				firstRest = fm
				if t := strictSystemHoistToTextContent(fm["content"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
	}
	for _, i := range offendingIndices {
		mm, _ := messages[i].(map[string]any)
		var content any
		if mm != nil {
			content = mm["content"]
		}
		if t := strictSystemHoistToTextContent(content); t != "" {
			texts = append(texts, t)
		}
	}
	// :57 `.join("\n")`
	mergedText := strings.Join(texts, "\n")

	// :59-62 rest[0] 原是 system → 就地合并进它
	if firstRestIsSystem {
		mergedFirst := make(map[string]any, len(firstRest))
		for k, v := range firstRest {
			mergedFirst[k] = v
		}
		mergedFirst["content"] = mergedText
		out := make([]any, 0, len(rest))
		out = append(out, mergedFirst)
		out = append(out, rest[1:]...)
		return out
	}

	// :64-65 没有前置 system → 造一个全新的置顶 system
	leadingSystem := map[string]any{"role": "system", "content": mergedText}
	out := make([]any, 0, len(rest)+1)
	out = append(out, leadingSystem)
	out = append(out, rest...)
	return out
}

// jsStringifyOrEmpty 复刻 JS 的 `String(x ?? "")`:
// undefined / null → ""; 字符串原样; 其余走 JS 的字符串化。
//
// 在本模块只会遇到 string 或 nil, 故只覆盖这两种 + 保守兜底。
func jsStringifyOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	// 数字 / 布尔在参考实现里会被 String() 转过。非字符串的 text 字段属畸形输入,
	// 我方保守返回空串而不是插入 Go 的格式化表示(避免引入参考实现没有的形态)。
	return ""
}

// strictSystemProviderIDsFromEnv 是调用方读取环境变量后的扩展清单入口。
// 属我方新增的接线辅助, 判定逻辑完全在 systemMessageMustBeFirst 内。
func strictSystemProviderIDs() []string {
	return parseStrictSystemProvidersEnv(getStrictSystemProvidersEnv())
}
