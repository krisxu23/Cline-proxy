package app

// 非 SSE 上游响应的流式化 (方案参照 OmniRoute 的 open-sse/utils/jsonToSse.ts,
// MIT 协议; 我们按 Go 侧语境重写实现):
//
// 问题: 部分"声称 OpenAI 兼容"的上游会忽略请求里的 stream:true, 直接回一个
// 完整的 application/json chat-completion body(实测 B.AI 的图片响应, 内含
// 巨型 C2PA _manifest)。我们的流式中继只认 `data:` 帧, 对非 data 行走"原样
// 透传"分支 → 客户端拿到裸 JSON 报 "JSON parsing failed"。
//
// 处理: 中继开头做一次形态判定 ——
//   - 首行是完整 JSON 且有 choices  → NDJSON 模式: 逐行转成 data: 帧;
//   - 首行不是完整 JSON(多行/美化) → 缓冲整个 body, 解析后合成等价 SSE 流;
//   - 首行是 data:/event:/注释       → 正常 SSE, 走原有中继逻辑。
// 合成时保留 content / reasoning_content / reasoning_details / tool_calls /
// finish_reason / usage, 与 OmniRoute 的合成语义对齐。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// jsonBodyMaxBytes 非 SSE body 的缓冲上限(防护: 异常上游灌大文件)。
const jsonBodyMaxBytes = 64 << 20

// looksLikeJSONBody 首行是否像是"非 SSE 的上游 JSON"(用于中继形态判定)。
func looksLikeJSONBody(firstLine string) bool {
	t := strings.TrimSpace(firstLine)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "data:") || strings.HasPrefix(t, "event:") || strings.HasPrefix(t, ":") {
		return false
	}
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// synthesizeOpenAISSEFromJSON 把完整 chat-completion JSON 合成为等效 SSE 流。
// 返回 (SSE 字节, 是否成功)。解析失败或不是 chat-completion 形状时返回 false,
// 调用方回退到原有(错误)处理。
func synthesizeOpenAISSEFromJSON(body []byte) ([]byte, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	choices, _ := parsed["choices"].([]any)
	if len(choices) == 0 {
		return nil, false
	}

	id, _ := parsed["id"].(string)
	if id == "" {
		id = "chatcmpl-synth-sse"
	}
	model, _ := parsed["model"].(string)
	var sb strings.Builder

	emit := func(obj map[string]any) {
		obj["id"] = id
		obj["object"] = "chat.completion.chunk"
		obj["model"] = model
		if b, err := json.Marshal(obj); err == nil {
			sb.WriteString("data: ")
			sb.Write(b)
			sb.WriteString("\n\n")
		}
	}

	for i, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		idx := i
		if v, ok := ch["index"].(float64); ok {
			idx = int(v)
		}
		msg, _ := ch["message"].(map[string]any)
		if msg == nil {
			// 兼容已带 delta 的形态
			msg, _ = ch["delta"].(map[string]any)
		}
		delta := map[string]any{}
		if idx == 0 {
			delta["role"] = "assistant"
		}
		if msg != nil {
			for _, k := range []string{"content", "reasoning_content", "reasoning", "reasoning_details", "tool_calls"} {
				if v, ok := msg[k]; ok && v != nil {
					delta[k] = v
				}
			}
		}
		emit(map[string]any{"created": parsed["created"], "choices": []any{
			map[string]any{"index": idx, "delta": delta},
		}})

		finish, _ := ch["finish_reason"].(string)
		emit(map[string]any{"created": parsed["created"], "choices": []any{
			map[string]any{"index": idx, "delta": map[string]any{}, "finish_reason": finish},
		}})
	}

	if u, ok := parsed["usage"].(map[string]any); ok && len(u) > 0 {
		emit(map[string]any{"created": parsed["created"], "choices": []any{}, "usage": u})
	}
	sb.WriteString("data: [DONE]\n\n")
	return []byte(sb.String()), true
}

// ndjsonLineToSSE 把 NDJSON 流式上游的单行 JSON 转成 data: 帧。
// 返回 (帧, 是否为可识别的 JSON 对象行)。
func ndjsonLineToSSE(line string) ([]byte, bool) {
	t := strings.TrimSpace(line)
	if t == "" {
		return nil, false
	}
	if strings.HasPrefix(t, "data:") || strings.HasPrefix(t, "[DONE]") {
		return nil, false
	}
	if !json.Valid([]byte(t)) {
		return nil, false
	}
	return []byte("data: " + t + "\n\n"), true
}

// sseSynthesisLog 供中继打点(便于诊断上游形态)。
func sseSynthesisLog(kind string, bytes int) string {
	return fmt.Sprintf("  stream: 上游以 %s 形态返回非 SSE 数据(%d 字节), 已合成为标准 SSE 流", kind, bytes)
}
