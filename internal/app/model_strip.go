package app

import "strings"

// 逐字照抄 OmniRoute:
//
//	open-sse/services/modelStrip.ts（60 行）
//
// 业务价值：模型能力表里的 `strip` 字段列出该模型**不支持**的内容类型
// （image / audio）。不剥的话，上游直接 400 拒绝（"model does not support
// images"），agent 客户端表现为任务无声中断 —— 与用户报的"无缘无故中断"
// 同源。
//
// 接入位置：providers_chat.go:chatWithKey，在拿到 p.name（真实 provider）
// 与 params["model"]（原始模型 id）之后、marshal 之前。
//
// ★ 有意不抄的部分：`getStripTypesForProviderModel` 依赖
// `PROVIDER_ID_TO_ALIAS` + `getModelStripTypes`（providerModels.ts:199-207），
// 后者读的是参考实现的**供应商模型注册表**（PROVIDER_MODELS Proxy），
// 本网关有自己的 providers_catalog.go 模型表。我方**自带 strip 配置**
// （见下方 stripTypesForModel），不照抄参考实现的注册表查询链路。

// stripTypesForModel 返回该模型应剥离的内容类型列表。
//
// ★ 照抄参考实现的**语义**（modelStrip.ts:9-17 的 shouldStripPart 逻辑），
//
//	但配置数据来自我方的模型表而非参考实现的 PROVIDER_MODELS Proxy。
//
// 判据（与参考实现 stripTypes 的语义一致）：
//   - "image" → 剥掉 type 为 "image_url" 或 "image" 的 content part
//   - "audio" → 剥掉 type 为 "input_audio" 或 "audio" 的 content part
//
// ★ 当前我方网关的模型（Claude / Gemini / OpenAI / Zen / OpenRouter /
//
//	TokenRouter / B.AI / Hermes）全部支持图像，故本函数目前恒返回空。
//	保留结构是为了后续接入不支持图像的模型（如某些 Gemini Flash 变体
//	或第三方 provider 的纯文本模型）时可以直接在此处加条目。
func stripTypesForModel(provider, model string) []string {
	// 当前所有已接入的模型都支持图像与音频，无需剥离。
	// 后续若接入纯文本模型，在此处加条目即可，例如：
	//   if strings.Contains(model, "flash-lite") && provider == "gemini" {
	//       return []string{"audio"}
	//   }
	_ = provider
	_ = model
	return nil
}

// stripIncompatibleMessageContent 照抄 modelStrip.ts:19-55。
//
// 参考实现原文（逐字）：
//
//	function shouldStripPart(part: JsonRecord, stripTypes: Set<string>): boolean {
//	  const type = typeof part.type === "string" ? part.type : "";
//	  if (!type) return false;
//	  if (stripTypes.has(type)) return true;
//	  if (stripTypes.has("image") && (type === "image_url" || type === "image")) return true;
//	  if (stripTypes.has("audio") && (type === "input_audio" || type === "audio")) return true;
//	  return false;
//	}
//	export function stripIncompatibleMessageContent(
//	  messages: unknown, stripTypes: readonly string[]
//	): { messages: unknown; removedParts: number } { ... }
//
// Go 侧差异：
//   - `Set<string>` → `map[string]bool`（Go 无 Set）
//   - `{ messages, removedParts }` → `(messages []any, removedParts int)`
//   - 入参 `messages` 是 `[]any`（Go 侧已解析过 JSON）
//   - `content` 过滤后为空时补一个占位文本（与参考实现 :48-51 一致）
func stripIncompatibleMessageContent(messages []any, stripTypes []string) ([]any, int) {
	if len(messages) == 0 || len(stripTypes) == 0 {
		return messages, 0
	}
	stripSet := make(map[string]bool, len(stripTypes))
	for _, s := range stripTypes {
		stripSet[s] = true
	}

	removedParts := 0
	out := make([]any, 0, len(messages))

	for _, msg := range messages {
		rec, ok := msg.(map[string]any)
		if !ok {
			out = append(out, msg)
			continue
		}
		content, isArr := rec["content"].([]any)
		if !isArr {
			out = append(out, msg)
			continue
		}

		filtered := make([]any, 0, len(content))
		for _, part := range content {
			if shouldStripPart(part, stripSet) {
				removedParts++
				continue
			}
			filtered = append(filtered, part)
		}

		if len(filtered) > 0 {
			next := make(map[string]any, len(rec))
			for k, v := range rec {
				next[k] = v
			}
			next["content"] = filtered
			out = append(out, next)
		} else {
			// 照抄 :48-51 —— 全部剥光时补一个占位文本，避免上游收到空 content。
			next := make(map[string]any, len(rec))
			for k, v := range rec {
				next[k] = v
			}
			next["content"] = []any{map[string]any{
				"type": "text",
				"text": "[unsupported image/audio content removed]",
			}}
			out = append(out, next)
		}
	}

	return out, removedParts
}

// shouldStripPart 照抄 modelStrip.ts:9-17。
func shouldStripPart(part any, stripSet map[string]bool) bool {
	rec, ok := part.(map[string]any)
	if !ok {
		return false
	}
	typeStr, _ := rec["type"].(string)
	typeStr = strings.TrimSpace(typeStr)
	if typeStr == "" {
		return false
	}
	if stripSet[typeStr] {
		return true
	}
	if stripSet["image"] && (typeStr == "image_url" || typeStr == "image") {
		return true
	}
	if stripSet["audio"] && (typeStr == "input_audio" || typeStr == "audio") {
		return true
	}
	return false
}
