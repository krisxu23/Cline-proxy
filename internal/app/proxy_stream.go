package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"free-router/internal/kit"
	"free-router/internal/protocol"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// readStreamLine 读一行(含行尾), 语义对齐 bufio.Reader.ReadString('\n') ——
// EOF/底层错误时返回已读前缀 + err。区别: 单行长度受 jsonBodyMaxBytes 约束
// (终审 P1)。此前四处 ReadString 均无上限, 上游持续推不带换行的字节会让
// 缓冲单调增长直到 OOM(字节一直在到达, idle 超时不触发); 现与 io.ReadAll /
// providerResponseMaxBytes 同用仓内既有的 64MiB 口径, 超限返回读错误,
// 各调用方按既有"非 EOF 错误→断流收尾"处理。
func readStreamLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > jsonBodyMaxBytes {
			return string(buf), fmt.Errorf("stream line exceeds %d bytes", jsonBodyMaxBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}

// handleStreamResponseWithUsage 返回最终交付状态(终审 P2): 空流守卫/内部
// 失败按 502 回传, 正常收尾 200 —— 调用方在 tracker.finish 前同步, 否则
// zen-stats 把失败记成成功(status 仍停留在 resp.StatusCode=200)。
func handleStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) int {
	return handleStreamResponseWithToolNameMap(w, upstream, onUsage, nil)
}

// handleStreamResponseWithToolNameMap 是 OpenAI 形态流式回写的实际实现，
// 多一个可选的工具名还原映射。
//
// 出处: OmniRoute handlers/responseTranslator.ts:165/173
// `restoreOpenAIToolNames(responseBody, toolNameMap)` 的**流式对位** —— 参考实现
// 在 OpenAI 形态出站时把 `choices[].delta.tool_calls[].function.name` 上的别名
// 换回客户端声明的原名（toolCallHelper.ts:131-157）。
//
// ★ 为什么 OpenAI 形态也需要还原: 请求侧的形状是**上游**决定的（claude /
//
//	anthropic-compatible-* 都要 cloak），而与客户端形态无关。若客户端是 OpenAI
//	形态而只做了 cloak 不做还原，它会收到自己从未声明过的工具名 → 调用静默失败。
//	toolNameMap 为 nil 时逐字节等价于旧行为。
func handleStreamResponseWithToolNameMap(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any), toolNameMap *toolNameMap) int {
	// Flusher 断言必须在 WriteHeader 之前(与 anthropic 路径同序): 否则不支持 Flush
	// 的 writer 会先拿到已提交的空 200 流, 再也到不了 500。
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming not supported for client"})
		return http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w)
	w.WriteHeader(http.StatusOK)

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
	// forwardedValuableChunk 对齐 OmniRoute open-sse/utils/stream.ts 的同名状态:
	// 只要有一帧带 content / tool_calls / finish_reason 被转发给客户端, 就置真。
	// 收尾时若它仍为假, 说明整条流没有交付任何有价值内容 —— 上游可能每帧都是
	// 空 choices。这种"干净的空 200"会被客户端当成一次合法的空回合, 于是静默
	// 结束任务且不会重试(2026-09-16 实测: 用户表现为"任务无缘无故中断, 没有
	// 任何提示也没有报错")。必须改判成可见的失败。
	forwardedValuableChunk := false
	// hasValidUsage 对齐 OmniRoute streamEmptyChoices.ts 的 ctx.hasValidUsage:
	// 上游如果报告了真实 token 用量, 说明这一回合在上游侧**确实发生过**,
	// 即便没有转发任何有价值 chunk 也不算空流(OmniRoute 原注释:
	// "usage-only streams are fine")。纯 usage 的收尾帧是正常协议行为,
	// 不是静默中断, 不能误杀。
	hasValidUsage := false
	// sawLegitEmptyTerminal 对齐 OmniRoute createStreamContentWatcher 的
	// sawLegitEmptyTerminal(): 一旦见到"合法空终止态"(finish_reason 为
	// length / tool_calls / content_filter, 或 stop_reason 为 max_tokens /
	// tool_use), 就说明这一回合**本就不该有正文** —— 纯工具调用回合、被
	// token 上限截断的回合都是合法的成功完成。此时空内容不是故障, 不能报 502。
	// 照抄 open-sse/utils/streamReadiness.ts:166 LEGIT_EMPTY_TERMINAL_REASONS。
	sawLegitEmptyTerminal := false
	// textualToolCalls 对齐 OmniRoute stream.ts:735 起的 passthroughToolCalls:
	// 承载从正文里解析出来的文本形态工具调用(键为调用序号)。
	textualToolCalls := map[string]textualToolCallRecord{}
	// allowedToolNames 对齐 stream.ts:299 extractAllowedToolNames(body):
	// 只接受客户端**确实声明过**的工具名, 防止正文里偶然出现的
	// "[Tool call: xxx]" 被误当成真实调用。
	// nil 语义与参考实现一致(:319-320) —— 没有工具声明时不做白名单过滤。
	allowedToolNames := extractAllowedToolNames(streamRequestTools(upstream))
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
				// 注意: 这个终止 chunk 是**我们合成的**, 绝不能计入
				// forwardedValuableChunk —— 否则上游"一帧内容都没发、只发了
				// [DONE]"的空流会因为这一帧被判成"有交付价值", 防护当场失效。
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
			if u, ok := obj["usage"].(map[string]any); ok && hasOutputUsageTokens(u) {
				hasValidUsage = true
			}
			if m, ok := obj["model"].(string); ok && m != "" {
				lastModel = m
			}
			normalized := normalizeOpenAIResponse(obj)
			// 工具名还原（照抄 toolCallHelper.ts:131-157 `restoreOpenAIToolNames`）。
			//
			// 位置: 必须在 normalize 之后、marshal 之前 —— 此时 `choices[].delta`
			// 已是标准形态，别名也仍在上游回显的位置上。还原是**幂等**的
			// （原样再跑一次不会改动），故对未 cloak 的请求零代价。
			if toolNameMap != nil && toolNameMap.len() > 0 {
				restoreOpenAIToolNames(normalized, toolNameMap)
			}
			if !sawFinish && protocol.HasStopSignal(normalized) { // 跨协议终止判定(OmniRoute checkIfStopSignal 等价)
				sawFinish = true
			}
			// 文本形态工具调用收集, 对齐 OmniRoute open-sse/utils/stream.ts:2506-2529。
			//
			// 部分上游(gemini 系/若干中转)不返回结构化 tool_calls, 而是把调用写进
			// 正文: "[Tool call: read_file]\nArguments: {...}"。不识别的话这段文本会
			// 原样透传给 agent —— 既不会执行工具也不报错, 表现就是**任务无声中断**。
			//
			// 三个分支逐条对齐参考实现:
			//   :2521-2526 collect 成功 → 从正文摘除, 并置 hasToolCalls
			//   :2527-2528 collect 失败但形态畸形 → 清空正文(避免把畸形标记喂给 agent)
			if collectTextualToolCalls(normalized, textualToolCalls, allowedToolNames) {
				forwardedValuableChunk = true
			}
			// 对齐 OmniRoute: 该帧是否值得转发由它是否带 content / tool_calls /
			// finish_reason 决定。三者都没有(空 choices)的帧仍然照常透传, 但它
			// 不计入"已交付有价值内容"。
			//
			// ★ 判据用 chunkDeliversUserContent(流级口径), **不是**参考实现的
			//   hasValuableContent(帧级口径)。后者把 role / finish_reason 也算
			//   有价值 —— 那对"这一帧要不要发给客户端"是对的, 对"整条流有没有
			//   产出"是错的(2026-09-17 审查 P0-1)。详见 stream_delivery.go。
			if chunkDeliversUserContent(normalized) {
				forwardedValuableChunk = true
			}
			if legitEmptyTerminalReason(normalized) {
				sawLegitEmptyTerminal = true
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
	//
	// 注意: 这两条合成路径同样要计入 forwardedValuableChunk —— 空流判定必须
	// 覆盖所有转发路径, 否则"上游回一个空壳 JSON"会绕过防护, 退回成客户端
	// 眼中的"干净空回合"(正是要根治的静默中断)。
	if firstLine, ferr := readStreamLine(reader); looksLikeJSONBody(firstLine) {
		trimmedFirst := strings.TrimSpace(firstLine)
		if json.Valid([]byte(trimmedFirst)) {
			// 判别"完整 JSON body"与"NDJSON 首行": 两者首行都是合法 JSON。
			// 区别在于**是不是一次完整回包** —— 非流式回包的 choices[].message
			// 形态与逐行 chunk 的 choices[].delta 形态是不同的。
			//   - 完整回包(choices 里有 message, 或带 object=chat.completion)
			//     → 走下面的完整 JSON body 合成路径;
			//   - 逐行 chunk(choices 里有 delta)
			//     → 走 NDJSON 路径。
			// 旧代码不做区分, 单行完整回包会被误当成 NDJSON 逐行透传: 它的
			// message 形态经 ndjsonLineToSSE 原样包成 data 帧后, delta 缺失,
			// hasValuableContent 判否 —— 一个**正常有内容的回包**会被空流防护
			// 误杀成 502(Test空流_完整JSON有内容正常通过 抓出的缺陷)。
			if !looksLikeFullCompletionBody(trimmedFirst) {
				// NDJSON 模式: 逐行转 data: 帧
				log.Printf("%s", sseSynthesisLog("NDJSON", len(firstLine)))
				// 解析前先剥掉 SSE `data:` 前缀(P2-23③): 上游可能在 NDJSON
				// 流里混发带 data: 前缀的行, 旧实现直接丢弃 → 混合行内容丢失。
				stripData := func(s string) string {
					t := strings.TrimSpace(s)
					if strings.HasPrefix(t, "data:") {
						return strings.TrimSpace(t[5:])
					}
					return t
				}
				markValuable := func(line string) {
					var probe map[string]any
					if json.Unmarshal([]byte(line), &probe) == nil {
						if chunkDeliversUserContent(probe) {
							forwardedValuableChunk = true
						}
						if legitEmptyTerminalReason(probe) {
							sawLegitEmptyTerminal = true
						}
						if u, ok := probe["usage"].(map[string]any); ok && len(u) > 0 {
							// 流式 usage 必须入账(P2-23①): 旧实现只捕获
							// hasValidUsage 给空流判定用, 全程不调 onUsage
							// → NDJSON 路径的 token 用量永远漏记。
							if onUsage != nil {
								onUsage(u)
							}
							if hasOutputUsageTokens(u) {
								hasValidUsage = true
							}
						}
					}
				}
				markValuable(trimmedFirst)
				if frame, ok := ndjsonLineToSSE(firstLine); ok {
					hb.writeFlush(frame)
				}
				for {
					line, lerr := readStreamLine(reader)
					if t := stripData(line); t != "" {
						markValuable(t)
						if t == "[DONE]" {
							// 旧实现在这里就发 [DONE], 之后收尾又补 finish chunk +
							// 第二个 [DONE] —— 多数客户端在首个 DONE 后停止解析,
							// finish chunk 直接丢失。这里只标记不发, 统一在 finish
							// 之后发**唯一一次** [DONE]。
							continue
						} else if frame, ok := ndjsonLineToSSE(t); ok {
							hb.writeFlush(frame)
						}
					}
					if lerr != nil {
						break
					}
				}
				if streamDeliveryEmpty(forwardedValuableChunk, hasValidUsage, sawLegitEmptyTerminal) {
					log.Printf("  stream: NDJSON 流未交付任何有价值内容, 回 error 帧而非静默空 200")
					writeStreamEmptyContentError(w, hb, lastModel)
					return http.StatusBadGateway
				}
				// 合成 finish chunk 之后才发 [DONE], 且**全程只发这一次**(P2-23②)。
				if b, mErr := json.Marshal(protocol.EmptyOpenAIChunk("").Payload); mErr == nil {
					hb.writeFlush([]byte("data: " + string(b) + "\n\n"))
				}
				hb.writeFlush([]byte("data: [DONE]\n\n"))
				return http.StatusOK
			}
		}
		// 多行 / 单行完整 JSON body: 缓冲全部后解析合成
		rest, _ := io.ReadAll(io.LimitReader(reader, jsonBodyMaxBytes))
		body := append([]byte(firstLine), rest...)
		if sse, ok := synthesizeOpenAISSEFromJSON(body); ok {
			log.Printf("%s", sseSynthesisLog("完整 JSON body", len(body)))
			hb.writeFlush(sse)
			// 合成的完整 JSON 同样是"是否交付了价值内容"的依据: 上游可能回一个
			// choices 里只有空 message 的壳。
			//
			// 判定对象是**原始 body 的实际内容**, 不是合成后的 SSE 帧 ——
			// synthesizeOpenAISSEFromJSON 会额外注入两种脚手架帧:
			//   1. delta.role = "assistant"(idx==0 必补);
			//   2. 带 finish_reason 的终止帧(每个 choice 都补)。
			// 而 hasValuableContent 显式接受 role 与 finish_reason, 于是拿合成帧
			// 去判会让"content 为空、只补了 role + finish"的空壳 body 被判成
			// 有交付价值, 空流防护当场失效。必须按原始 body 里**真实存在的**
			// 正文/工具调用判定(Test空流_完整JSON空壳body必须失败 抓出的缺陷)。
			//
			// 终审 P3: 合成已保证 body 可解析, 此处**只 parse 一次**复用于
			// onUsage / hasValidUsage / 内容 / 白名单四个判定 —— 此前同一份
			// 最高 64MiB 的 body 被反复全量 Unmarshal(~6 次)。
			parsedBody, _ := parseJSONMap(body)
			if onUsage != nil {
				if u, ok := parsedBody["usage"].(map[string]any); ok && len(u) > 0 {
					onUsage(u)
				}
			}
			if u, ok := parsedBody["usage"].(map[string]any); ok && hasOutputUsageTokens(u) {
				hasValidUsage = true
			}
			if fullCompletionBodyHasContent(parsedBody) {
				forwardedValuableChunk = true
			}
			// 合法空终止态同样要按**原始 body** 判定(理由同上: 合成器补的终止帧
			// 会把任意空壳 body 都带上 finish_reason="stop", 而 "stop" 不在白名单里,
			// 所以只有原始 body 真实携带 length/tool_calls 等才算数)。
			if legitEmptyTerminalReason(parsedBody) {
				sawLegitEmptyTerminal = true
			}
			if streamDeliveryEmpty(forwardedValuableChunk, hasValidUsage, sawLegitEmptyTerminal) {
				log.Printf("  stream: 上游完整 JSON body 不含任何有价值内容, 补 error 帧")
				writeStreamEmptyContentError(w, hb, lastModel)
				return http.StatusBadGateway
			}
			return http.StatusOK
		}
		// 合不成: firstLine 已被消费, 主循环不会再看到它 —— 终审 P3: 显式走
		// 一遍统一行处理器(能按 NDJSON 抢救的抢救, 不能的丢弃并留日志),
		// 不再与注释里"交给主循环按坏行门卫处理"的承诺相悖地静默消失。
		handleLine(firstLine, ferr == io.EOF)
	} else if ferr == nil || (ferr == io.EOF && firstLine != "") {
		// 首行是正常 SSE: 走统一实现处理一次再进主循环(F2 修复点 —— 此处
		// 旧代码是独立的精简版循环体, 现收敛)。
		//
		// 首行遇到 EOF 且非 JSON-looking(上游回无换行的单行非 JSON body:
		// 纯文本/HTML 错误页)时也必须无条件处理一次(P2-25): 否则首行内容
		// 不进任何分支, 主循环随后读到 "" + EOF 直接 break —— 响应体被整体
		// 吞掉, 客户端只拿到空流 502, 上游错误信息无从排查。
		// 正常(非 EOF)首行仍走 ferr == nil 分支, 行为不变。
		handleLine(firstLine, ferr == io.EOF)
	}

	for {
		// 客户端已断开(broken pipe)时不再读上游: 粘性写错误一旦置位,
		// 后续写必败, 继续读只是把上游流量往黑洞里倒。
		if hb.WriteErr() != nil {
			break
		}
		line, err := readStreamLine(reader)
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
		if hb.WriteErr() != nil {
			break
		}
	}

	// 空流拒绝(对齐 OmniRoute open-sse/utils/streamEmptyChoices.ts 的
	// rejectEmptyChoicesStream): 整条流没交付任何有价值 chunk 时, 不能以
	// "干净的 200" 收尾。客户端会把这种空回合当成合法结果 —— 不报错、不重试,
	// 直接静默结束任务, 用户看到的就是"无缘无故中断"。
	//
	// 与上游"空流"的区别: 那种在提交前就被 probeStreamFirstEvent 拦下并换站
	// (见 routing_dispatch.go), 这里兜的是**已提交之后**每帧都空的情况。
	if streamDeliveryEmpty(forwardedValuableChunk, hasValidUsage, sawLegitEmptyTerminal) {
		log.Printf("  stream: 上游整条流未交付任何有价值内容(model=%s, 已发 finish=%v), 回 502 而非静默空 200",
			lastModel, sawFinish)
		writeStreamEmptyContentError(w, hb, lastModel)
		return http.StatusBadGateway
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
	return http.StatusOK
}

// 注(2026-09-17 审查 P0-1): 此处原有 openAIChunkHasValuableContent 与
// streamDeliveredValue 两个函数, 已删除。
//
// 删除原因: 它们把**帧级转发过滤**的判据(role / finish_reason 也算"有价值")
// 用在了**流级产出判定**上。参考实现 OmniRoute utils/streamHelpers.ts:379 的
// hasValuableContent 用于 stream.ts:1803 `if (!hasValuableContent(...)) continue;`
// —— 回答的是"这一帧要不要发给客户端", 那里 role / finish_reason 当然算有价值
// (客户端靠它们收尾)。但拿它回答"整条流有没有产出"是错的: 一条只发了
// `{"delta":{"role":"assistant"}}` 或 `{"delta":{},"finish_reason":"stop"}`
// 就结束的流, 对用户等于零产出。
//
// 实测(临时用例, 已删): role 帧 / finish_reason 帧 / 空格 reasoning /
// 只有 prompt_tokens 的 usage —— 四种形态全部穿过守卫。
//
// 唯一判据现集中在 stream_delivery.go:
//   chunkDeliversUserContent  逐帧判"是否携带用户可见产出"
//   hasOutputUsageTokens      只认输出侧 token
//   streamDeliveryEmpty       空流守卫的唯一聚合入口

// legitEmptyTerminalReasons 照抄 OmniRoute open-sse/utils/streamReadiness.ts:166:
//
//	// Terminal states where a completion legitimately carries no content, kept in
//	// step with errorClassifier.ts's LEGIT_EMPTY_OPENAI_FINISH / LEGIT_EMPTY_CLAUDE_STOP
//	// so the streaming and non-streaming empty-content checks agree.
//	const LEGIT_EMPTY_TERMINAL_REASONS = new Set([
//	  "length", "tool_calls", "content_filter", "max_tokens", "tool_use",
//	]);
//
// 对应的权威来源 open-sse/services/errorClassifier.ts:14-15:
//
//	const LEGIT_EMPTY_CLAUDE_STOP = new Set(["max_tokens", "tool_use"]);
//	const LEGIT_EMPTY_OPENAI_FINISH = new Set(["length", "tool_calls", "content_filter"]);
//
// 原注释点明用途: 被 token 上限截断(finish_reason="length")、或纯工具调用回合
// (finish_reason="tool_calls")的空内容, 是**合法的成功完成**, 不是静默假成功。
// 不加这条白名单会把合法的 HTTP 200 改写成合成的 502 —— 例如 Claude Code 的
// max_tokens:1 连通性探测, 以及 agent 工具调用回合(该回合本来就只有
// tool_calls 而无正文文本)。
var legitEmptyTerminalReasons = map[string]bool{
	"length":         true,
	"tool_calls":     true,
	"content_filter": true,
	"max_tokens":     true,
	"tool_use":       true,
}

// legitEmptyTerminalReason 判一个 chunk 是否带"合法空终止态"的终止原因。
// 照抄 OmniRoute TERMINAL_REASON_PATTERN / LEGIT_EMPTY_TERMINAL_REASONS 语义:
// 只认 finish_reason(OpenAI)与 stop_reason(Claude)两个字段名。
func legitEmptyTerminalReason(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	if ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]any); ok {
			if fr, ok := first["finish_reason"].(string); ok && legitEmptyTerminalReasons[fr] {
				return true
			}
		}
	}
	// Anthropic 形态的顶层 stop_reason(经 normalizeOpenAIResponse 后仍可能保留)
	if sr, ok := obj["stop_reason"].(string); ok && legitEmptyTerminalReasons[sr] {
		return true
	}
	return false
}

// 注(2026-09-17 审查 P0-1): 此处原有 hasValidUsageTokens, 已删除。
//
// 它的判据是"已知 token 字段任一 > 0"(照抄 OmniRoute usageTracking.ts:639),
// 其中包含 prompt_tokens / input_tokens / promptTokenCount 三个**输入侧**字段。
// 输入有值只说明上游收到了我们的 prompt, 完全不能说明它产出了东西 ——
// {prompt_tokens:1500, completion_tokens:0} 正是最典型的真·空回包, 却被判成
// "已交付", 于是空流守卫被绕过。
//
// 替代: stream_delivery.go 的 hasOutputUsageTokens —— 只认输出侧
// (completion_tokens / output_tokens / candidatesTokenCount)。

// looksLikeFullCompletionBody 判断一个合法 JSON 对象是不是"一次完整回包"
// (非流式 chat.completion), 而不是 NDJSON 的一行流式 chunk。
//
// 判据(任一命中即认为是完整回包):
//   - object 字段等于 "chat.completion"(非 ".chunk");
//   - choices[] 里出现 message 形态(非流式回包用它承载正文)。
func looksLikeFullCompletionBody(payload string) bool {
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return false
	}
	if objType, ok := obj["object"].(string); ok {
		if objType == "chat.completion" || strings.HasSuffix(objType, ".completion") {
			return true
		}
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, hasMessage := choice["message"]; hasMessage {
			return true
		}
	}
	return false
}

// fullCompletionBodyHasContent 判断"一次完整回包"的 body 里是否真有内容。
//
// 不看脚手架(role / finish_reason —— 这两样合成器会补、且 hasValuableContent
// 显式接受), 只看 body 里**真实承载输出**的字段:
//   - choices[].message.content 非空;
//   - choices[].message 带非空 reasoning 家族字段;
//   - choices[].message.tool_calls 非空;
//   - choices[].text(旧版 text_completion 形态)非空。
//
// 全都没有 → 上游交付的是一个空壳。
func fullCompletionBodyHasContent(parsed map[string]any) bool {
	choices, ok := parsed["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := choice["text"].(string); ok && len(text) > 0 {
			return true
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			// 已带 delta 的形态也接受(合成器兼容这种输入)
			msg, _ = choice["delta"].(map[string]any)
		}
		if msg == nil {
			continue
		}
		if c, ok := msg["content"].(string); ok && len(c) > 0 {
			return true
		}
		// content 数组形态(OpenAI content-parts / Anthropic blocks 经翻译后):
		// 任一块带非空 text 即视为有内容。否则一个带 content[] 的完整 JSON
		// body 会被判成空壳, 对正常回包误打 502(2026-09-17 审查 R2-6)。
		if blocks, ok := msg["content"].([]any); ok && len(blocks) > 0 {
			for _, rawBlock := range blocks {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := block["text"].(string); ok && len(t) > 0 {
					return true
				}
			}
		}
		if hasAnyReasoningSignal(msg) {
			return true
		}
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			return true
		}
	}
	return false
}

// parseJSONMap 把完整 JSON body 解析成 map, 供"合法空终止态"判定使用。
func parseJSONMap(body []byte) (map[string]any, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

// parsedUsageFromJSONBody 从完整 JSON body 里取 usage。
func parsedUsageFromJSONBody(body []byte) (map[string]any, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	u, ok := parsed["usage"].(map[string]any)
	if !ok || len(u) == 0 {
		return nil, false
	}
	return u, true
}

// writeStreamEmptyContentError 交付"可见的空内容失败"。
//
// 帧里同时带 error(能弹提示的客户端看得到原因)与 finish_reason(只认协议
// 终止信号的客户端也能正常收尾), 再补 [DONE]。HTTP 状态码在流式提交后无法
// 再改, 因此失败信息只能走 SSE 帧 —— 这正是 OmniRoute 用 controller.error
// 表达 502 的等价做法。
func writeStreamEmptyContentError(w http.ResponseWriter, hb *sseHeartbeat, model string) {
	payload := map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"error": map[string]any{
			"type": "empty_content",
			"message": "上游未返回任何内容(整条流无有效 chunk)。这通常是出口节点或上游 worker " +
				"异常所致, 请重试; 若持续出现请更换出口节点。",
		},
	}
	if model != "" {
		payload["model"] = model
	}
	if b, err := json.Marshal(payload); err == nil {
		hb.write([]byte("data: " + string(b) + "\n\n"))
	}
	hb.write([]byte("data: [DONE]\n\n"))
	hb.flush()
}

func handleNonStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) int {
	return handleNonStreamResponseWithToolNameMap(w, upstream, onUsage, nil)
}

// handleNonStreamResponseWithToolNameMap 是 OpenAI 形态非流式回写的实际实现，
// 多一个可选的工具名还原映射（出处与流式版同: responseTranslator.ts:165/173
// `restoreOpenAIToolNames`）。返回最终交付状态(终审 P2): 内部写 500/502 时
// 调用方必须同步给 tracker, 否则 zen-stats 把失败记成成功。
func handleNonStreamResponseWithToolNameMap(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any), toolNameMap *toolNameMap) int {
	// 非流式响应同样要过控制字符清洗 —— 此前只有流式路径有清洗器, 于是上游
	// (实测 B.AI 图片响应的 C2PA _manifest)忽略 stream:true 直接回完整 JSON,
	// 或流式被掏空退化成裸 body 时, 字符串里的裸控制字符会原样直达客户端,
	// 报 "Bad control character in string literal"(2026-09-15 事故次生缺陷)。
	rawBody, readErr := io.ReadAll(io.LimitReader(upstream.Body, providerResponseMaxBytes+1))
	if readErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": readErr.Error(), "type": "parse_error"},
		})
		return http.StatusInternalServerError
	}
	if len(rawBody) > providerResponseMaxBytes {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": "upstream response exceeds size limit", "type": "api_error"},
		})
		return http.StatusBadGateway
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
				return http.StatusInternalServerError
			}
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return http.StatusInternalServerError
		}
	}

	// Some Cline responses wrap in {data: {...}} / {message: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	// usage 必须在解包**之后**再取(与 handleLine 的 F2 顺序一致, 见上方注释):
	// 旧实现先取顶层 raw["usage"] 再解包, 于是 usage 位于 data 载荷或 message
	// 内层时永远取不到 → stats / 账户配额 / request-log 全记 0(P1-14)。
	// 逐层查找并保证只调一次 onUsage: 解包后的载荷 → 载荷的 message 内层 →
	// 外层原包 → 外层 message 内层。
	if onUsage != nil {
		pick := func(m map[string]any) map[string]any {
			if u, ok := m["usage"].(map[string]any); ok && len(u) > 0 {
				return u
			}
			return nil
		}
		var u map[string]any
		for _, m := range []map[string]any{out, raw} { // out 与 raw 相同时等价于只查一层
			if u = pick(m); u != nil {
				break
			}
			if msg, ok := m["message"].(map[string]any); ok {
				if u = pick(msg); u != nil {
					break
				}
			}
		}
		if u != nil {
			onUsage(u)
		}
	}

	out = normalizeOpenAIResponse(out)

	// 工具名还原（照抄 toolCallHelper.ts:131-157 `restoreOpenAIToolNames`）。
	// 位置与参考实现一致: 在 `choices[].message.tool_calls` 已就位、即将写出之前。
	if toolNameMap != nil && toolNameMap.len() > 0 {
		restoreOpenAIToolNames(out, toolNameMap)
	}

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
	return http.StatusOK
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
	sawDataLine := false
	for {
		line, err := readStreamLine(reader)
		if err != nil {
			if err != io.EOF {
				// 终审 P1: 非 EOF 读错误(context 取消/连接重置/idle 超时)必须
				// 上抛 —— 此前只 break 再无条件 return nil, 半截正文会按 200
				// 成功交付, 无法与完整回答区分(静默中断类别)。
				return nil, err
			}
		}
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			sawDataLine = true
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
	// 终审 P1: EOF 且从未见任何 data 行 = 上游一个 chunk 都没发, 判空上抛,
	// 调用方已有 err→500 分支(proxy.go / anthropic.go / responses.go / zen)。
	if !sawDataLine {
		return nil, fmt.Errorf("upstream stream ended without any data frame")
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
