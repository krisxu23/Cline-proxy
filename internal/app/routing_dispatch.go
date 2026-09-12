package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// 候选链调度: 按顺序尝试每一站, 失败按类别记冷却并前进, 全链失败返回最后一站错误。
//
// failover 的时机是"首字节之前" —— 每一站都是先拿到 *http.Response、
// 判定状态码与内容, 确认可用后才开始写 w。因此:
//   - 非流式: 可以先读完 body 判定"200 但无内容", 不合格就换下一站;
//   - 流式  : 200 即开始透传, 之后中断按现有语义终止(不再换站)。
//
// 这样客户端只会看到最终胜出那一站的响应, 中间失败的站点对它是不可见的 ——
// 也不会出现"已经回了一部分再用另一站重来"的破损响应。

// chainShape 客户端期望的响应形状。上游一律返回 OpenAI 形状,
// 是否需要转换由入口决定(/chat/completions 不需要, /v1/messages 与
// /v1/responses 需要)。
type chainShape int

const (
	shapeOpenAI chainShape = iota
	shapeAnthropic
	shapeResponses
)

// chainTarget 描述"把胜出那一站的响应以什么形状回给客户端"。
type chainTarget struct {
	Shape       chainShape
	ToolSchemas map[string]map[string]bool
}

// chainFailoverHeader 命中候选链时回填的路由头取值, 便于在请求日志里区分。
const chainFailoverHeader = "chain"

// handleChainedChat 执行候选链(OpenAI 形状入口)。
func handleChainedChat(w http.ResponseWriter, r *http.Request, params map[string]any, chain []routeCandidate, requested string) {
	handleChainedChatAs(w, r, params, chain, requested, chainTarget{})
}

// handleChainedChatAs 执行候选链, 按 tgt 决定回写形状。
func handleChainedChatAs(w http.ResponseWriter, r *http.Request, params map[string]any, chain []routeCandidate, requested string, tgt chainTarget) {
	isStream, _ := params["stream"].(bool)
	applyOverride(params)

	var (
		lastErr    error
		lastStatus = http.StatusBadGateway
		skipped    int
		tried      int
	)
	for _, cand := range chain {
		if why := candidateSkip(cand); why != "" {
			skipped++
			log.Printf("  chain: 跳过 %s (%s)", cand.String(), why)
			continue
		}
		tried++
		hopParams := cloneParamsForCandidate(params, cand)
		model, _ := hopParams["model"].(string)

		resp, err := callChainUpstream(r.Context(), cand, hopParams, isStream)
		if err != nil {
			// 通用 Provider 把非 200 直接返回为错误(而不是响应), 所以状态码必须
			// 从错误里取出来 —— 否则限流/下架会被误判成超时, 只短冷却一次。
			status, body := chainErrorStatusBody(err)
			class, reason := classifyCandidateFailure(status, body)
			lastErr, lastStatus = err, upstreamErrorStatus(err)
			applyCandidateFailure(cand, class, reason, body)
			recordUsageForCandidate(cand, false)
			log.Printf("  chain: %s 失败(%s), 换下一站: %v", cand.String(), class, err)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			if !isStream {
				// 非流式要先确认"真的带了内容": 上游用 200 + 空壳表示模型暂不可用时,
				// 直接回给客户端只会让它以为成功。
				body, rerr := io.ReadAll(io.LimitReader(resp.Body, providerResponseMaxBytes+1))
				resp.Body.Close()
				if rerr != nil || int64(len(body)) > providerResponseMaxBytes {
					lastErr = fmt.Errorf("%s: reading response: %v", cand.String(), rerr)
					lastStatus = http.StatusBadGateway
					markCandidateCooldown(cand.Upstream, cand.Model, classTimeout, "读取响应失败")
					recordUsageForCandidate(cand, false)
					continue
				}
				if !chatBodyHasContent(body) || chainBodyOnlyBrokenToolCalls(body) {
					lastErr = fmt.Errorf("%s: HTTP 200 with no usable content", cand.String())
					lastStatus = http.StatusBadGateway
					reason := "200 但无内容"
					if chainBodyOnlyBrokenToolCalls(body) {
						// 带了 tool_calls 但 id/name 缺到一个都不剩: 对 agent 客户端
						// 等价于空响应, 换下一站往往能拿到完整调用
						reason = "200 但 tool_calls 不完整"
					}
					markCandidateCooldown(cand.Upstream, cand.Model, classEmpty, reason)
					recordUsageForCandidate(cand, false)
					log.Printf("  chain: %s %s, 换下一站", cand.String(), reason)
					continue
				}
				tracker := newZenStatsTracker(zenStatsRecord{
					TS:           time.Now().UnixMilli(),
					Upstream:     chainUpstreamLabel(cand),
					Model:        model,
					Stream:       false,
					PromptTokens: estimateJSON(hopParams),
				})
				setRouteHeader(w, chainUpstreamLabel(cand), model, chainFailoverHeader)
				writeChainNonStream(w, body, tgt, tracker)
				tracker.finish(true, http.StatusOK)
				recordUsageForCandidate(cand, true)
				logChainResult(cand, tried, skipped)
				return
			}

			tracker := newZenStatsTracker(zenStatsRecord{
				TS:           time.Now().UnixMilli(),
				Upstream:     chainUpstreamLabel(cand),
				Model:        model,
				Stream:       true,
				PromptTokens: estimateJSON(hopParams),
			})
			setRouteHeader(w, chainUpstreamLabel(cand), model, chainFailoverHeader)
			switch tgt.Shape {
			case shapeAnthropic:
				handleAnthropicStreamWithUsage(w, resp, model, tgt.ToolSchemas, tracker.observeUsage)
			case shapeResponses:
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusOK)
				chatStreamToResponses(w, resp, nil)
			default:
				handleStreamResponseWithUsage(w, resp, tracker.observeUsage)
			}
			tracker.finish(resp.StatusCode < 400, resp.StatusCode)
			recordUsageForCandidate(cand, true)
			logChainResult(cand, tried, skipped)
			return
		}

		// 非 200: 读出错误体判定类别, 再决定冷却方式
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastErr = &upstreamError{Upstream: cand.Upstream, Status: resp.StatusCode, Body: string(body)}

		class, reason := classifyCandidateFailure(resp.StatusCode, body)
		applyCandidateFailure(cand, class, reason, body)
		recordUsageForCandidate(cand, false)
		log.Printf("  chain: %s HTTP %d(%s), 换下一站", cand.String(), resp.StatusCode, class)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("route %q: no candidate could be tried", requested)
		lastStatus = http.StatusServiceUnavailable
	}
	log.Printf("  chain: %q 全部候选失败 (%d 尝试, %d 跳过): %v", requested, tried, skipped, lastErr)
	status := lastStatus
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": fmt.Sprintf("route %q: all candidates failed; last error: %v", requested, lastErr),
			"type":    "upstream_unavailable",
		},
	})
}

func logChainResult(cand routeCandidate, tried, skipped int) {
	if tried > 1 || skipped > 0 {
		log.Printf("  chain: 命中 %s (第 %d 次尝试, 跳过 %d 站)", cand.String(), tried, skipped)
	}
}

// cloneParamsForCandidate 复制参数并把模型名换成该站的模型。
// 必须复制: 候选之间模型名不同, 就地修改会让后续站点看到错误的名字。
func cloneParamsForCandidate(params map[string]any, cand routeCandidate) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out["model"] = candidateModelName(cand)
	return out
}

// candidateModelName 该站实际发给上游的模型名。
// cline 池的 "*" 是占位: 具体模型由默认模型决定, 池内轮询负责选账号。
func candidateModelName(cand routeCandidate) string {
	if cand.Model == clinePoolPlaceholder {
		return getDefaultModel()
	}
	return cand.Model
}

// chainUpstreamLabel 统计与路由头用的上游名: provider 记为 provider/<name>,
// 与既有 stats 的 upstream 常量保持同一套口径。
func chainUpstreamLabel(cand routeCandidate) string {
	switch cand.Upstream {
	case upstreamZen:
		return upstreamZen
	case upstreamCline:
		return upstreamCline
	default:
		return providerUpstream(cand.Upstream)
	}
}

// resetTZFor Google 系候选按太平洋时间重置, 其余按本地时区。
func resetTZFor(upstream string) string {
	if candidateUsesPacificReset(upstream) {
		return pacificTZ
	}
	return "Asia/Shanghai"
}

// quotaDayExhausted 429 是否属于"当日额度耗尽"(而非分钟级限流)。
func quotaDayExhausted(body []byte) bool {
	qf := parseQuotaFailure(jsonObject(body))
	return qf != nil && qf.ExhaustedWindow == "day"
}

// applyCandidateFailure 把一次失败落到具体的冷却动作上。三个归宿:
//   - permanent: 永久剔除(无免费层 / 已下架 / 非 chat 模型), 不再消耗候选位;
//   - quotaDay : 当日免费额度耗尽 -> 冷却到提供方时区的日界;
//   - 其余类别 : 按配置或默认时长冷却。
func applyCandidateFailure(cand routeCandidate, class, reason string, body []byte) {
	key := candidateKey(cand.Upstream, cand.Model)
	// Gemini 的回包里带了"每日限额"就回填账本: 之后到量即跳过,
	// 不必等到真的撞一次 429 才知道用完。
	if qf := parseQuotaFailure(jsonObject(body)); qf != nil && qf.DailyRequestLimit != nil {
		autoFillDailyLimit(key, *qf.DailyRequestLimit)
	}
	switch {
	case class == classPermanent:
		markCandidatePermanent(cand.Upstream, cand.Model, reason)
	case class == classRateLimit && quotaDayExhausted(body):
		markQuotaDayCooldown(cand.Upstream, cand.Model, resetTZFor(cand.Upstream), reason)
	default:
		markCandidateCooldown(cand.Upstream, cand.Model, class, reason)
	}
}

// callChainUpstream 按上游类别发起一次请求。
func callChainUpstream(ctx context.Context, cand routeCandidate, params map[string]any, stream bool) (*http.Response, error) {
	switch cand.Upstream {
	case upstreamZen:
		resp, _, err := callZenAPI(ctx, params, stream)
		return resp, err
	case upstreamCline:
		resp, _, err := callClineAPIFailover(params, stream)
		return resp, err
	default:
		p := providerByName(cand.Upstream)
		if p == nil {
			return nil, fmt.Errorf("provider %s is not configured", cand.Upstream)
		}
		return p.Chat(ctx, params, stream)
	}
}

// jsonObject 尽力把 body 解成一个 JSON 对象, 供配额解析复用。
func jsonObject(body []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return m
}

// writeChainNonStream 把胜出那一站的 200 响应按目标形状回写。
//
// body 已完整读过(非流式路径), 所以既可以直接透传 OpenAI 形状,
// 也可以转成 Anthropic 形状 —— 上游永远只会返回 OpenAI 形状。
func writeChainNonStream(w http.ResponseWriter, body []byte, tgt chainTarget, tracker *zenStatsTracker) {
	if tgt.Shape == shapeOpenAI {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}
		handleNonStreamResponseWithUsage(w, resp, tracker.observeUsage)
		return
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": "upstream returned unparsable JSON", "type": "parse_error"},
		})
		return
	}
	chatOut := raw
	if d, ok := raw["data"].(map[string]any); ok {
		chatOut = d
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	if u, ok := chatOut["usage"].(map[string]any); ok && len(u) > 0 {
		tracker.observeUsage(u)
	}
	if tgt.Shape == shapeResponses {
		writeJSON(w, http.StatusOK, chatToResponses(chatOut))
		return
	}
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
}

// chatBodyHasContent 200 响应是否真的带了内容(spec 的 empty 类别判据)。
//
// 有些上游用 200 + 空壳(choices 为空, 或 content 为空且没有 tool_calls)
// 表示"这个模型现在不可用"。这类响应既不该回给客户端,
// 也不该让该候选留在链首反复被选中。
func chatBodyHasContent(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	out := raw
	if d, ok := raw["data"].(map[string]any); ok {
		out = d
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	if s, ok := getNested(out, "choices", 0, "text").(string); ok && s != "" {
		return true
	}
	msg, _ := getNested(out, "choices", 0, "message").(map[string]any)
	if msg == nil {
		return false
	}
	if s, _ := msg["content"].(string); s != "" {
		return true
	}
	if tc, _ := msg["tool_calls"].([]any); len(tc) > 0 {
		return true
	}
	return false
}

// chainBodyOnlyBrokenToolCalls 响应声称发起了工具调用, 但修补后一条都不剩
// (全部缺 function.name, 缺 id 的会被补全所以不算)。对 agent 客户端而言这
// 等价于空响应且更糟 —— 客户端会因校验失败直接报错, 因此与空内容同罪,
// 候选链换下一站。
func chainBodyOnlyBrokenToolCalls(body []byte) bool {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	out := raw
	if d, ok := raw["data"].(map[string]any); ok {
		out = d
	}
	msg, _ := getNested(out, "choices", 0, "message").(map[string]any)
	if msg == nil {
		return false
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) == 0 {
		return false
	}
	repaired, _ := repairToolCalls(tcs)
	return len(repaired) == 0
}
