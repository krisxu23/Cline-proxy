package app

import (
	"bufio"
	"cline-go-proxy/internal/kit"
	"cline-go-proxy/internal/protocol"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func handleStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	// 上游流空闲保护(P2, 参照 OmniRoute 的流式 idle 机制): 正文阶段挂起时
	// 主动断开, 由收尾逻辑合成 finish/[DONE], 避免客户端无限等待。
	idleRC := newIdleAbortReader(upstream.Body, streamIdleTimeout())
	defer idleRC.Close()

	// 控制字符清洗放在行切分之前(见 controlSanitizingReader 注释)。
	// 注意: 这里**不再**包 heartbeatReader —— 心跳已移到输出侧(见下), 绝不能在
	// 上游字节流里掺任何帧, 否则会把分片传输的巨型 JSON 帧拦腰截断(实测事故)。
	src := io.Reader(&controlSanitizingReader{src: idleRC})
	reader := bufio.NewReader(src)

	// 流式保活(P1-11, 输出侧, 参照 OmniRoute 的 earlyStreamKeepalive): 距上次向
	// 客户端写出真实字节超过间隔时, 向客户端写一个协议合法的空 delta 帧。心跳与
	// 上游数据是两条永不相交的流 —— 从根上杜绝旧 heartbeatReader 的截帧缺陷。
	hb := newSSEHeartbeat(w, flusher, streamHeartbeatInterval(), func() []byte { return openAIHeartbeatFrame })
	defer hb.Close()

	// 统一行处理器(F2 收敛): 主循环与"首行是正常 SSE"共用**同一份**实现,
	// 并以闭包更新循环状态(sawFinish/sawDone/lastModel)。返回 true 表示
	// 残行(residual)处理完毕、读取应结束。旧实现在首行分支里养了一份
	// "精简版"循环体 —— 不走 normalize、不记 usage、不更新 sawDone(首行即
	// [DONE] 时收尾会重复补终止帧)、坏行静默吞掉 —— 同一逻辑两份实现必然
	// 漂移(R2 审计 F2), 现收敛为单一实现。
	sawFinish := false // 上游是否已发过 finish_reason
	sawDone := false   // 上游是否已发过 [DONE]
	lastModel := ""    // 用于兜底 chunk 的 model 字段
	handleLine := func(line string, residual bool) bool {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" {
				hb.writeFlush([]byte(line + "\n\n"))
				return residual
			}
			if payload == "[DONE]" {
				// 上游结束但从未给出 finish_reason: 补一个终止 chunk
				if !sawFinish {
					if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk(lastModel).Payload); mErr == nil {
						hb.write([]byte("data: " + string(b) + "\n\n"))
					}
				}
				sawDone = true
				hb.writeFlush([]byte(line + "\n\n"))
				return residual
			}

			// Try to normalize the response
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				// 坏行门卫(P2 修复): 上游会送来两类脏 JSON ——
				//   1) 被截断/交错的行(实测 {"choices"0}],...);
				//   2) 字符串里夹裸控制字符的行(实测 _manifest/C2PA 图片元数据,
				//      报 "Bad control character in string literal")。
				// 先尝试清洗控制字符后重试(能救回来就不丢内容), 仍失败才丢弃。
				if fixed, ok := sanitizeJSONControlChars([]byte(payload)); ok {
					if err := json.Unmarshal(fixed, &obj); err == nil {
						log.Printf("  stream: 上游 data 行含非法控制字符, 已清洗救回(%d 字节)", len(payload))
					} else {
						log.Printf("  stream: 丢弃无法解析的上游 data 行(%d 字节): %q", len(payload), kit.Truncate(payload, 120))
						return residual
					}
				} else {
					log.Printf("  stream: 丢弃无法解析的上游 data 行(%d 字节): %q", len(payload), kit.Truncate(payload, 120))
					return residual
				}
			}
			// Some Cline responses wrap in {data: {...}} —— 先解包裹再记 usage:
			// 旧顺序先查外层 obj["usage"], 导致包裹形态的 usage 永远漏记
			// (Dashboard 少账, F2 收敛时一并修正)。
			if data, ok := obj["data"]; ok {
				if d, ok := data.(map[string]any); ok {
					if _, hasChoices := d["choices"]; hasChoices {
						obj = d
					}
					if _, hasID := d["id"]; hasID {
						obj = d
					}
				}
			}
			if onUsage != nil {
				if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
					onUsage(u)
				}
			}
			if m, ok := obj["model"].(string); ok && m != "" {
				lastModel = m
			}
			normalized := normalizeOpenAIResponse(obj)
			if !sawFinish && protocol.HasStopSignal(normalized) { // 跨协议终止判定(OmniRoute checkIfStopSignal 等价)
				sawFinish = true
			}
			if normBytes, err := json.Marshal(normalized); err == nil {
				hb.writeFlush([]byte("data: " + string(normBytes) + "\n\n"))
				return residual
			}
			// normalize 序列化失败: 继续落入下方非 data 分支尝试 NDJSON 抢救(与旧实现一致)
		}

		// 非 data 行: 只透传 SSE 合法帧结构(空行分隔符 / event: / id: / retry: /
		// 注释行)。其余一律视为损坏内容 —— 先按 NDJSON 转帧抢救, 抢救不动就丢弃。
		// 旧实现把任意非 data 行原样写回客户端, 裸 _manifest 分片和黏合帧正是这样
		// 直达客户端报 "Bad control character" 的(2026-09-15 事故复盘)。
		if line == "" {
			hb.writeFlush([]byte("\n"))
			return residual
		}
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") ||
			strings.HasPrefix(line, "retry:") || strings.HasPrefix(line, ":") {
			hb.writeFlush([]byte(line + "\n"))
			return residual
		}
		if frame, ok := ndjsonLineToSSE(line); ok {
			hb.writeFlush(frame)
			return residual
		}
		log.Printf("  stream: 丢弃非 SSE 帧的上游损坏行(%d 字节): %q", len(line), kit.Truncate(line, 120))
		return residual
	}

	// 形态判定(P2, 参照 OmniRoute open-sse/utils/jsonToSse.ts):
	// 上游可能忽略 stream:true 直接回完整 JSON, 或按 NDJSON 逐行回 JSON。
	// 这两类都不是 SSE —— 原样透传会让客户端报 "JSON parsing failed"。
	// 首行探测: 非 data:/event:/注释 且以 { [ 开头 → 走合成路径。
	if firstLine, ferr := reader.ReadString('\n'); looksLikeJSONBody(firstLine) {
		if json.Valid([]byte(strings.TrimSpace(firstLine))) {
			// NDJSON 模式: 逐行转 data: 帧
			log.Printf("%s", sseSynthesisLog("NDJSON", len(firstLine)))
			if frame, ok := ndjsonLineToSSE(firstLine); ok {
				hb.writeFlush(frame)
			}
			for {
				line, lerr := reader.ReadString('\n')
				if t := strings.TrimSpace(line); t != "" {
					if frame, ok := ndjsonLineToSSE(t); ok {
						hb.writeFlush(frame)
					} else if t == "[DONE]" {
						hb.writeFlush([]byte("data: [DONE]\n\n"))
					}
				}
				if lerr != nil {
					break
				}
			}
			if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk("").Payload); mErr == nil {
				hb.writeFlush([]byte("data: " + string(b) + "\n\n"))
			}
			hb.writeFlush([]byte("data: [DONE]\n\n"))
			return
		}
		// 多行 JSON body: 缓冲全部后解析合成
		rest, _ := io.ReadAll(io.LimitReader(reader, jsonBodyMaxBytes))
		body := append([]byte(firstLine), rest...)
		if sse, ok := synthesizeOpenAISSEFromJSON(body); ok {
			log.Printf("%s", sseSynthesisLog("完整 JSON body", len(body)))
			hb.writeFlush(sse)
			if onUsage != nil {
				var parsed map[string]any
				if json.Unmarshal(body, &parsed) == nil {
					if u, ok := parsed["usage"].(map[string]any); ok && len(u) > 0 {
						onUsage(u)
					}
				}
			}
			return
		}
		// 合不成: 交给主循环按坏行门卫处理(不再原样透传裸 JSON)
		if _, ok := ndjsonLineToSSE(firstLine); !ok {
			log.Printf("  stream: 上游返回无法识别的非 SSE 数据(%d 字节首行), 将按坏行丢弃而非透传", len(firstLine))
		}
	} else if ferr == nil {
		// 首行是正常 SSE: 走统一实现处理一次再进主循环(F2 修复点 —— 此处
		// 旧代码是独立的精简版循环体, 现收敛)。
		handleLine(firstLine, false)
	}

	for {
		line, err := reader.ReadString('\n')
		// EOF 时残行(最后一帧不带换行)也要走统一处理, 不能裸写回客户端。
		if err != nil && err != io.EOF {
			break
		}
		residual := err == io.EOF && strings.TrimRight(line, "\r\n") != ""
		if err == io.EOF && !residual {
			break
		}
		if handleLine(line, residual) {
			break
		}
	}

	// 上游断流未发 [DONE](或发 [DONE] 前无 finish_reason): 合成收尾,
	// 避免客户端报 "Stream ended without finish_reason" 或挂起等待。
	if !sawFinish {
		if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk(lastModel).Payload); mErr == nil {
			hb.write([]byte("data: " + string(b) + "\n\n"))
		}
	}
	if !sawDone {
		hb.write([]byte("data: [DONE]\n\n"))
	}
	hb.flush()
}

func handleNonStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	// 非流式响应同样要过控制字符清洗 —— 此前只有流式路径有清洗器, 于是上游
	// (实测 B.AI 图片响应的 C2PA _manifest)忽略 stream:true 直接回完整 JSON,
	// 或流式被掏空退化成裸 body 时, 字符串里的裸控制字符会原样直达客户端,
	// 报 "Bad control character in string literal"(2026-09-15 事故次生缺陷)。
	rawBody, readErr := io.ReadAll(io.LimitReader(upstream.Body, providerResponseMaxBytes+1))
	if readErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": readErr.Error(), "type": "parse_error"},
		})
		return
	}
	if len(rawBody) > providerResponseMaxBytes {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": "upstream response exceeds size limit", "type": "api_error"},
		})
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err != nil {
		// 直解失败: 先做位置感知清洗(字符串内裸控制字符→空格)再重试, 与流式坏行
		// 门卫同一判定。救不回才报错 —— 此时报错也优于把含裸控制字符的坏 JSON 透传。
		if fixed, ok := sanitizeJSONControlChars(rawBody); ok {
			if err2 := json.Unmarshal(fixed, &raw); err2 == nil {
				log.Printf("  nonstream: 上游 JSON 含非法控制字符, 已清洗救回(%d 字节)", len(rawBody))
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "parse_error"},
				})
				return
			}
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
	}

	if onUsage != nil {
		if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
			onUsage(u)
		}
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

func collectStreamResponse(upstream *http.Response) (map[string]any, error) {
	var (
		model        string
		content      strings.Builder
		finishReason string
		usage        map[string]any
		toolCalls    []any
		toolCallIdx  = -1
		curToolCall  map[string]any
		curArgs      strings.Builder
	)

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF && err != bufio.ErrBufferFull {
				break
			}
		}
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				if err == io.EOF {
					break
				}
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if data, ok := obj["data"]; ok {
				if d, ok := data.(map[string]any); ok {
					obj = d
				}
			}
			if m, ok := obj["model"].(string); ok && m != "" {
				model = m
			}
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				usage = u
			}
			choices, _ := getNested(obj, "choices").([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				delta = choice
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if c, ok := delta["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if tcRaw, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcRaw {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					idx := 0
					if i, ok := tcMap["index"].(float64); ok {
						idx = int(i)
					}
					if idx != toolCallIdx {
						if curToolCall != nil {
							curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
							toolCalls = append(toolCalls, curToolCall)
						}
						curToolCall = map[string]any{
							"id":       tcMap["id"],
							"type":     "function",
							"function": map[string]any{"name": "", "arguments": ""},
						}
						curArgs.Reset()
						toolCallIdx = idx
					}
					if fn, ok := tcMap["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							curToolCall["function"].(map[string]any)["name"] = n
						}
						if a, ok := fn["arguments"].(string); ok && a != "" {
							curArgs.WriteString(a)
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if curToolCall != nil {
		curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
		toolCalls = append(toolCalls, curToolCall)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}
	out := map[string]any{
		"id":      "chatcmpl_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

func modelNeedsStream(modelID string) bool {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.RequiresStream {
		return true
	}
	return false
}
