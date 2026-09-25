package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// 候选链调度: 按顺序尝试每一站, 失败按类别记冷却并前进, 全链失败返回最后一站错误。
//
// failover 的时机是"首字节之前" —— 每一站都是先拿到 *http.Response、
// 判定状态码与内容, 确认可用后才开始写 w。因此:
//   - 非流式: 可以先读完 body 判定"200 但无内容", 不合格就换下一站;
//   - 流式  : 提交前先探首个有效事件(probeStreamFirstEvent), 空流即换站;
//     提交之后中断按现有语义终止(不再换站)。
//
// ★ 2026-09-24 补上第三格: 首事件有效、**后续整条流却零产出**的空回包, 此前
// 流处理器只能把 502 交回这里, 而这里无条件 return —— 网关内部不换站。现在
// 只要**响应头尚未提交**(由 proxy_stream.go 的 lazyCommitWriter 保证, 见
// commitProbeWriter) 就继续换站, 预算见 emptyStreamRetryBudget。
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

// emptyStreamRetryBudget 空流(流内改判失败)换站重试的**独立时间预算** ——
// 2026-09-24 用户拍板 5 分钟。
//
// 为什么用总时长而不是次数上限: 每次空流的耗时差异极大(实测约 11 秒, 慢出口可达
// 数十秒), 次数上限给出的实际窗口会随上游状态剧烈漂移 —— 3 次可能 30 秒, 也可能
// 3 分钟。用户要的是"给 agent 一个确定的等待上限", 所以直接卡总时长。
//
// 为什么独立: 不复用网络错 / 429 的重试预算 —— 那两类各自已有自己的窗口, 叠加起来
// 会显著超过 5 分钟, 对 agent 来说和卡死没区别。
//
// 两个消费点, 各自一个**惰性起点**(第一次遇到空流才起算, 不累计请求前段耗时):
//   - 候选链: routing_dispatch.go 的 delivered>=400 分支(换**候选站**);
//   - 直连 zen: proxy_cline.go 的 handleZenStreamChat(换**出口**)。
//
// 声明为变量而不是常量: 测试要把它覆盖成**已超期**(负值)才能覆盖"预算耗尽 →
// 交还真错误"那条分支(本仓既有做法, 见 nodeHealthFileOverride / nodeStableLoaded)。
// 生产路径只读。
var emptyStreamRetryBudget = 5 * time.Minute

// emptyStreamExitCooldown 空回包后冷却"刚用过的那个出口"的时长。
//
// 取值与出口级通用冷却一致(拨号失败也是 2 分钟, 见 zenDialGuardedPinned)——
// 空回包说明这个出口到上游的这条线路上没拿到内容, 属于同一类"这个出口现在不行"。
// 冷却只影响选路(下一次 callZenAPI 会跳过它), 不写 quota 冷却表 —— 那不是额度问题。
const emptyStreamExitCooldown = 2 * time.Minute

// emptyStreamMaxExitRotations 一次请求内因空回包换出口的**次数上限**。
//
// 为什么在"5 分钟总时长"之外还要一个次数上限: 与 zenQuotaRotateBudget(429 换出口)
// 同一条理由 —— **每次换出口都要真发一次上游请求**。没有次数上限时, 一个"系统性
// 空回包"的模型(上游侧坏了, 换哪个出口都一样)会在 5 分钟里打上百次上游, 并且把
// 上百个健康出口逐个写进 2 分钟冷却表 ⇒ 后续请求无出口可用。
// 8 次 ≈ 20~25 秒(实测每次空回包约 2.8s), 仍在 agent 的请求超时之内。
//
// 两个上限谁先到算谁: 8 次通常先生效, 单次很慢时(如 40s/次)由 5 分钟先生效。
const emptyStreamMaxExitRotations = 8

// commitProbeWriter 记录"响应头是否已经提交" —— 空流换站重试的安全前提。
//
// ★ 为什么必须**观测**而不是按响应形状猜: 只有 OpenAI 形状的出站路径做了延迟提交
// (proxy_stream.go 的 lazyCommitWriter), 空流时响应头尚未提交, 换一站对客户端完全
// 无感。anthropic / responses 两条路径在进流处理器**之前**就 WriteHeader(200), 头一旦
// 提交就再也改不了口 —— 在那里换站会把第二次的内容接在第一次**已经发出去**的流后面
// (2026-09-24 用户明确警告过的顺序陷阱), 比不换更糟。
//
// 判据取自实际观测, 所以将来任何一条路径改成延迟提交, 空流换站会自动对它生效,
// 不需要同步改这里。
//
// committed 用 atomic: 心跳泵是独立 goroutine, 它可能经 hb.flush_ → lw.Flush →
// 本类型 Flush 写这个字段, 而请求 goroutine 会读它。
type commitProbeWriter struct {
	http.ResponseWriter
	committed atomic.Bool
}

func (c *commitProbeWriter) WriteHeader(code int) {
	c.committed.Store(true)
	c.ResponseWriter.WriteHeader(code)
}

func (c *commitProbeWriter) Write(b []byte) (int, error) {
	c.committed.Store(true)
	return c.ResponseWriter.Write(b)
}

// Flush 必须实现: 流式处理链会断言 http.Flusher, 缺失会让 handler 直接走
// "streaming not supported" 分支(500)。net/http 的 Flush 会**隐式提交**响应头,
// 因此它同样意味着"已提交"。
func (c *commitProbeWriter) Flush() {
	c.committed.Store(true)
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// handleChainedChat 执行候选链(OpenAI 形状入口)。
func handleChainedChat(w http.ResponseWriter, r *http.Request, params map[string]any, chain []routeCandidate, requested string) {
	handleChainedChatAs(w, r, params, chain, requested, chainTarget{})
}

// handleChainedChatAs 执行候选链, 按 tgt 决定回写形状。
func handleChainedChatAs(w http.ResponseWriter, r *http.Request, params map[string]any, chain []routeCandidate, requested string, tgt chainTarget) {
	isStream, _ := params["stream"].(bool)
	applyOverride(params)

	// 响应头提交探针(见 commitProbeWriter): 空流换站只在"响应头尚未提交"时安全。
	// 这里直接替换 w(而不是新增变量), 是为了保证下游**所有**回写都经过它 ——
	// 漏掉任何一处都会得出"未提交"的错判, 进而把第二次的内容接到第一次的流后面。
	probe := &commitProbeWriter{ResponseWriter: w}
	w = probe

	// 请求轨迹与决策轨迹: 一次链式调度同时回填"请求日志用的元数据"和
	// "面板详情用的逐候选决策"(参照 OmniRoute 的 call_logs + decisionTrace)。
	tr := traceFrom(r.Context())
	tr.SetProtocol(chainProtocolName(tgt.Shape), isStream)
	dec := decisionTraceStart(reqIDFrom(r.Context()), requested)
	defer dec.finish()

	// Task1 入口门禁: 池空 + 地区受限链拒绝静默直连(防 h2 池污染)。
	// 重建(loadSubCache/syncNodeBox)由 writePoolEmptyGate 内部完成。
	if chainNeedsPoolGate(chain, requested) && writePoolEmptyGate(w, r, requested) {
		return
	}

	var (
		lastErr    error
		lastStatus = http.StatusBadGateway
		skipped    int
		tried      int
	)
	// 空流换站的时间预算(见 emptyStreamRetryBudget 的完整理由)。
	//
	// 起点是**第一次遇到空流**的时刻(惰性), 不是请求开始: 请求在候选链上可能已经
	// 花掉很久(前面几站各吃一次超时), 那段时间不属于"空流重试"。
	var emptyStreamDeadline time.Time
	// 统计估算整包 marshal 每请求只算一次: hopParams 与 params 只差 model 名,
	// 候选间复用同一估值(真实 usage 回来后 observeUsage 会覆盖)。
	promptTokens := estimateJSON(params)
	for _, cand := range chain {
		if why := candidateSkip(cand); why != "" {
			skipped++
			tr.AddSkip(cand.String(), why)
			dec.addCandidate(cand.String(), "skipped", why, 0, "")
			log.Printf("  chain: 跳过 %s (%s)", cand.String(), why)
			continue
		}
		tried++
		tr.AddAttempt()
		hopParams := cloneParamsForCandidate(params, cand)
		model, _ := hopParams["model"].(string)

		resp, err := callChainUpstream(r.Context(), cand, hopParams, isStream)
		if err != nil {
			// 通用 Provider 把非 200 直接返回为错误(而不是响应), 所以状态码必须
			// 从错误里取出来 —— 否则限流/下架会被误判成超时, 只短冷却一次。
			status, body := chainErrorStatusBody(err)
			class, reason := classifyCandidateFailure(status, body)
			lastErr, lastStatus = err, upstreamErrorStatus(err)
			recordChainTryFailure(cand, class, reason, status, body, dec)
			// 链失败路径必须落请求轨迹: tried 由循环顶记, skipped 由跳过分支记,
			// 此处补 error class —— 收尾的 tr.SetError 只记最后一站类别,
			// 逐站明细靠 tried/skipped 还原, 无静默丢弃。
			tr.SetError(class, reason)
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
					tr.AddAttempt()
					recordChainTryFailure(cand, classTimeout, "读取响应失败", 0, nil, dec)
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
					tr.AddAttempt()
					recordChainTryFailure(cand, classEmpty, reason, http.StatusOK, body, dec)
					log.Printf("  chain: %s %s, 换下一站", cand.String(), reason)
					continue
				}
				tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
					TS:           time.Now().UnixMilli(),
					Upstream:     chainUpstreamLabel(cand),
					Model:        model,
					Stream:       false,
					PromptTokens: promptTokens,
				})
				setRouteHeader(w, chainUpstreamLabel(cand), model, chainFailoverHeader)
				tr.SetUpstream(chainUpstreamLabel(cand), model)
				// 非流式: 上游 usage 由 writeChainNonStream 内部回填 tracker, 这里
				// 顺带把同一份 usage 记进请求轨迹(trim 面板要的 token 明细)。
				// 非流式同样需要工具名还原（对位 responseTranslator.ts:740 —— 那条
				// 路径同时覆盖"纯非流式请求"与"stream:true 但上游以 application/json 回"）。
				writeChainNonStreamWithMap(w, body, tgt, func(u map[string]any) {
					tracker.observeUsage(u)
					tr.ObserveUsage(u)
				}, takeToolNameMap(hopParams))
				tracker.finish(true, http.StatusOK)
				recordUsageForCandidate(cand, true)
				markCandidateSuccess(cand.Upstream, cand.Model)
				dec.addCandidate(cand.String(), "tried", "", http.StatusOK, "")
				dec.setWinner(cand.String())
				logChainResult(cand, tried, skipped)
				return
			}

			// 流式早断守卫(P2, 参照 OmniRoute 的 STREAM_EARLY_EOF 语义):
			// 上游回 200 + event-stream 却立即空流时, 一旦提交就只能把空答案
			// 交给客户端。提交前先探首个有效事件 —— 空流则按非流式路径同样的
			// 方式冷却本站并换下一站; 首 token 慢的上游不误杀(超时即放行)。
			//
			// ★ 判定强度已升级为 stream_readiness.go 的状态机（照抄
			//   streamReadiness.ts）：error-only 帧、ping 事件、空对象都
			//   不算就绪，避免空答案被当成功提交。
			empty, nb, upstreamDiag := probeStreamFirstEvent(resp.Body)
			if empty {
				resp.Body.Close()
				lastErr = fmt.Errorf("%s: HTTP 200 stream with no first event", cand.String())
				lastStatus = http.StatusBadGateway
				tr.AddAttempt()
				recordChainTryFailure(cand, classEmpty, "200 但空流", http.StatusOK, nil, dec)
				if upstreamDiag != "" {
					log.Printf("  chain: %s 200 但空流(early EOF), 上游诊断: %s, 换下一站", cand.String(), upstreamDiag)
				} else {
					log.Printf("  chain: %s 200 但空流(early EOF), 换下一站", cand.String())
				}
				continue
			} else {
				resp.Body = nb
			}

			tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
				TS:           time.Now().UnixMilli(),
				Upstream:     chainUpstreamLabel(cand),
				Model:        model,
				Stream:       true,
				PromptTokens: promptTokens,
			})
			setRouteHeader(w, chainUpstreamLabel(cand), model, chainFailoverHeader)
			tr.SetUpstream(chainUpstreamLabel(cand), model)
			// 工具名还原映射（照抄 OmniRoute chatCore.ts:2592-2610 的
			// `const toolNameMap = translatedBody._toolNameMap; delete ...`）。
			//
			// hopParams 就是被 chatWithKey 改写过的那个对象（同一引用）—— 请求侧
			// cloak 把 `_toolNameMap` 摘到旁路键后，这里读出并移除，交给响应侧的
			// Claude-shape 回写器把上游回显的别名换回客户端声明的原名。
			// 无伪装（纯 PascalCase 真 Claude Code 流量）时为 nil，行为不变。
			responseToolNameMap := takeToolNameMap(hopParams)

			observe := func(u map[string]any) {
				tracker.observeUsage(u)
				tr.ObserveUsage(u)
			}
			// 终审 P2: 交付状态回传 —— 空流守卫的 502 错误帧若记成 200,
			// 成功率指标与 request-log 互相矛盾。
			delivered := resp.StatusCode
			switch tgt.Shape {
			case shapeAnthropic:
				delivered = handleAnthropicStreamWithToolNameMap(w, resp, model, tgt.ToolSchemas, observe, responseToolNameMap)
			case shapeResponses:
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				setCORSOrigin(w)
				w.WriteHeader(http.StatusOK)
				delivered = chatStreamToResponses(w, resp, nil)
			default:
				delivered = handleStreamResponseWithToolNameMap(w, resp, observe, responseToolNameMap)
			}
			// ★ 流内空回包也要换站重试(2026-09-24, 用户规格: 上游的错误必须在网关
			//   内部消化, 不得让 agent 看到)。
			//
			//   delivered>=400 且**响应头未提交**表示流处理器已判定"整条流未交付任何
			//   有价值内容"(proxy_stream.go 的 lazyCommitWriter 保证此时一个字节都没
			//   发给客户端)。此前这里无条件 return —— 网关内部不换站, 客户端只能自己
			//   处理; 现在改成: 冷却本站(空流大概率是该出口/worker 异常) + 换下一站,
			//   全程对客户端无感。
			//
			//   两个安全前提, 缺一不可:
			//     1. !probe.committed.Load() —— 已提交就改不了口(见 commitProbeWriter);
			//     2. 在 emptyStreamRetryBudget 之内 —— 耗尽后不再换站, 让下面的收尾
			//        把真 502 交给客户端(用户要的"超时仍未成功 → 返回一个真错误")。
			if delivered >= 400 && !probe.committed.Load() {
				if emptyStreamDeadline.IsZero() {
					emptyStreamDeadline = time.Now().Add(emptyStreamRetryBudget)
				}
				// 客户端已经断开(agent 自己超时/取消)时不再换站: 再试也没人收,
				// 只会白白冷却后面的候选。
				if r.Context().Err() == nil && time.Now().Before(emptyStreamDeadline) {
					resp.Body.Close()
					lastErr = fmt.Errorf("%s: 流内空回包(首事件有效但整条流无有效 chunk)", cand.String())
					lastStatus = http.StatusBadGateway
					markCandidateCooldown(cand.Upstream, cand.Model, classEmpty, "流内空回包")
					recordUsageForCandidate(cand, false)
					dec.addCandidate(cand.String(), "tried", "流内空回包", resp.StatusCode, classEmpty)
					log.Printf("  chain: %s 流内空回包, 冷却本站并换下一站(空流预算剩余 %s)",
						cand.String(), time.Until(emptyStreamDeadline).Truncate(time.Second))
					continue
				}
				// 预算耗尽 / 客户端已走: 按失败记账后跳出, 交给链尾收尾返回真错误。
				//
				// 不能落到下面的"成功收尾"(tracker.finish / markCandidateSuccess):
				// 那会把刚打的冷却清掉、并把这次失败记成 winner, 请求日志与成功率
				// 指标都会与实际相反。
				resp.Body.Close()
				lastErr = fmt.Errorf("%s: 流内空回包且空流重试预算(%s)已耗尽或客户端已断开", cand.String(), emptyStreamRetryBudget)
				lastStatus = http.StatusBadGateway
				markCandidateCooldown(cand.Upstream, cand.Model, classEmpty, "流内空回包")
				recordUsageForCandidate(cand, false)
				dec.addCandidate(cand.String(), "tried", "流内空回包(预算耗尽)", resp.StatusCode, classEmpty)
				log.Printf("  chain: %s 流内空回包, 空流重试预算(%s)已耗尽或客户端已断开, 返回真 502 给客户端",
					cand.String(), emptyStreamRetryBudget)
				break
			}
			// 这里以前从不关闭上游响应体。对比上面两条失败路径(87/154 行)都显式
			// Close 了, 唯独流式成功这条漏掉, 而三个流式 handler 内部也都只读到
			// EOF、不负责 Close —— 连接因此无法归还复用池, 长时间运行持续堆积。
			resp.Body.Close()
			tracker.finish(delivered < 400, delivered)
			recordUsageForCandidate(cand, true)
			markCandidateSuccess(cand.Upstream, cand.Model)
			dec.addCandidate(cand.String(), "tried", "", resp.StatusCode, "")
			dec.setWinner(cand.String())
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
		dec.addCandidate(cand.String(), "tried", reason, resp.StatusCode, class)
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
	tr.SetError(errClassForStatus(status), lastErr.Error())
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": fmt.Sprintf("route %q: all candidates failed; last error: %v", requested, lastErr),
			"type":    "upstream_unavailable",
		},
	})
}

// chainProtocolName 把响应形状映射成客户端入口协议名(请求日志用)。
func chainProtocolName(s chainShape) string {
	switch s {
	case shapeAnthropic:
		return "anthropic"
	case shapeResponses:
		return "responses"
	default:
		return "openai"
	}
}

// errClassForStatus 请求日志用的粗粒度错误类别(链式调度全站失败时)。
func errClassForStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return classRateLimit
	case status >= 500:
		return classServerError
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth"
	case status >= 400:
		return "client_error"
	default:
		return classEmpty
	}
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

// applyCandidateFailure 把一次失败落到具体的冷却动作上。三个归宿:
//   - permanent: 永久剔除(无免费层 / 已下架 / 非 chat 模型), 不再消耗候选位;
//   - quotaDay : 当日免费额度耗尽 -> 冷却到提供方时区的日界;
//   - 其余类别 : 按配置或默认时长冷却。
func applyCandidateFailure(cand routeCandidate, class, reason string, body []byte) {
	// 硬失败记入模型可用性门(429/4xx 不计): 连续挂掉的模型自动从列表与选路中摘除。
	//
	// 只认两类**上游明确表达**的信号: 5xx(上游自己报错 —— classifyCandidateFailure
	// 已把未匹配的 4xx 归入 classClientError, 不再冒充 serverError 落到这里,
	// "4xx 不计"的承诺因此与实现一致, P2-9)与空响应(上游正常收尾但没产出内容)。
	// 超时/网络属线路问题 —— 把它算成模型硬失败, 会让出口池抖动期间
	// 健康模型被连续暂停 30 分钟并从列表消失(2026-09-16 实证)。
	if class == classServerError || class == classEmpty {
		recordZenModelResult(cand.Model, true)
	}
	key := candidateKey(cand.Upstream, cand.Model)
	// Gemini 的回包里带了"每日限额"就回填账本: 之后到量即跳过,
	// 不必等到真的撞一次 429 才知道用完。
	qf := parseQuotaFailure(jsonObject(body))
	if qf != nil && qf.DailyRequestLimit != nil {
		autoFillDailyLimit(key, *qf.DailyRequestLimit)
	}
	switch {
	case class == classPermanent:
		markCandidatePermanent(cand.Upstream, cand.Model, reason)
	case class == classRateLimit && qf != nil && qf.ExhaustedWindow == "day":
		markQuotaDayCooldown(cand.Upstream, cand.Model, resetTZFor(cand.Upstream), reason)
	default:
		// 上游 RetryInfo 给了明确延迟就覆盖类别默认时长: 分钟级 429 的真实
		// 窗口可能 <10min(过冷)或 >10min(反复白撞)。
		var retryMs int64
		if class == classRateLimit && qf != nil {
			retryMs = qf.RetryDelayMs
		}
		markCandidateCooldownFor(cand.Upstream, cand.Model, class, reason, retryMs)
	}
}

// callChainUpstream 按上游类别发起一次请求。
func callChainUpstream(ctx context.Context, cand routeCandidate, params map[string]any, stream bool) (*http.Response, error) {
	switch cand.Upstream {
	case upstreamZen:
		resp, _, err := callZenAPI(ctx, params, stream)
		return resp, err
	case upstreamCline:
		resp, _, err := callClineAPIFailover(ctx, params, stream)
		return resp, err
	case upstreamClinePass:
		// ClinePass 与 Cline 池是不同上游(独立 key 池与端点), 此前没接进候选链,
		// 结果是"四类上游只能组合三类"; 补上后订阅模型可以真正参与自动路由。
		return callClinePassChain(ctx, params, stream)
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
func writeChainNonStream(w http.ResponseWriter, body []byte, tgt chainTarget, observeUsage func(map[string]any)) {
	writeChainNonStreamWithMap(w, body, tgt, observeUsage, nil)
}

// writeChainNonStreamWithMap 带上工具名还原映射（对位参考实现
// `convertOpenAINonStreamingToClaude(openaiResponse, toolNameMap?)` 的可选尾参）。
func writeChainNonStreamWithMap(w http.ResponseWriter, body []byte, tgt chainTarget, observeUsage func(map[string]any), toolNameMap *toolNameMap) {
	if tgt.Shape == shapeOpenAI {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}
		// OpenAI 形态出站同样要还原别名（responseTranslator.ts:173）。
		handleNonStreamResponseWithToolNameMap(w, resp, observeUsage, toolNameMap)
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
	if u, ok := chatOut["usage"].(map[string]any); ok && len(u) > 0 && observeUsage != nil {
		observeUsage(u)
	}
	if tgt.Shape == shapeResponses {
		writeJSON(w, http.StatusOK, chatToResponses(chatOut))
		return
	}
	anthropicResp := openAIToAnthropicWithMap(chatOut, toolNameMap, tgt.ToolSchemas)
	// 覆写守卫(与 anthropic.go 同款): 只有映射结果仍是 end_turn 才覆写成 tool_use ——
	// finish_reason=length 映射出的 max_tokens 必须保留, 截断的半截工具参数伪装成
	// 完整调用会让客户端解析失败且无从重试。
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 &&
		anthropicResp["stop_reason"] == "end_turn" {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
}

// chatBodyHasContent 200 响应是否真的带了内容(spec 的 empty 类别判据)。
//
// 有些上游用 200 + 空壳(choices 为空, 或 content 为空且没有 tool_calls)
// 表示"这个模型现在不可用"。这类响应既不该回给客户端,
// 也不该让该候选留在链首反复被选中。
//
// ★ 2026-09-17 审查 P1-2 修正: 此前本函数**只认字符串 content + tool_calls**,
// 漏了 content 数组形态与 reasoning。而 proxy_stream.go 的
// fullCompletionBodyHasContent 已经补过数组形态(当时的理由: "带 content[] 的
// 正常回包会被误判成空壳、对正常回包误打 502")。
//
// 同一 bug 只修了被点名的那一条路径, 于是候选链这里仍会把带 content[] 的正常
// 回包判成"空内容"→ 换下一站。用户侧表现: **某个模型明明能答, 网关总说它空、
// 老是换模型**。
//
// 现在统一复用 stream_delivery.go 的 chunkDeliversUserContent —— 与流式路径
// 同一口径(遍历全部 choice; 认 content 字符串/数组、reasoning 各别名、tool_calls)。
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
	// completions API 形态(choices[].text): 统一判据不认这个字段, 单独判。
	if s, ok := getNested(out, "choices", 0, "text").(string); ok && s != "" {
		return true
	}
	return chunkDeliversUserContent(out)
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
