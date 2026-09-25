package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"free-router/internal/cline"
	"free-router/internal/kit"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// handleZenChat opencode zen 免费模型分支: 压缩 -> 上游 -> 透传,并记录统计
func handleZenChat(w http.ResponseWriter, r *http.Request, params map[string]any) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	zm, ok := resolveZenFreeModel(model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", model), "type": "invalid_request_error"},
		})
		return
	}
	isStream, _ := params["stream"].(bool)
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     upstreamZen,
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	sid := requestSessionID(params, r.Header)
	out := maybeCompact(r.Context(), params, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  zen: %s", out.note)
	}

	usageFn := func(u map[string]any) {
		// 镜像进请求轨迹(多协议字段名归一; 与 stats 记账互不影响)。
		tracker.trace.ObserveUsage(u)
		// completion_tokens 缺失时保持原值(P3-36): 旧实现先执行
		// `CompletionTokens += int(pt) - PromptTokens` 兜底, 而差值是
		// **prompt 的增量**、不是 completion 量 —— 输入一变长输出 token
		// 就被虚高。这里只认 completion_tokens, 缺就保持 0/入站估算。
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		// ★ 流式走独立循环(2026-09-24): 上游**空回包**可以在"响应头未提交"时隐形
		// 换出口重试 —— 见 handleZenStreamChat。候选链那条换站重试
		// (routing_dispatch.go 的 delivered>=400 分支)覆盖不到这条路径。
		st := handleZenStreamChat(w, r, params, tracker, usageFn)
		tracker.finish(st < 400, st)
		return
	}

	resp, rateLimited, err := callZenAPI(r.Context(), params, false)
	if err != nil {
		status := zenWriteUpstreamError(w, r, tracker, rateLimited, err)
		tracker.finish(false, status)
		return
	}
	tracker.rec.RateLimited += rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode
	st := handleNonStreamResponseWithUsage(w, resp, usageFn)
	tracker.finish(st < 400, st)
}

// zenWriteUpstreamError 直连 zen 路径把上游错误按既有口径回给客户端:
// 记日志 + 记账 + 落请求轨迹 + 写 JSON 错误体。返回交给 tracker.finish 的状态码。
func zenWriteUpstreamError(w http.ResponseWriter, r *http.Request, tracker *zenStatsTracker, rateLimited int, err error) int {
	log.Printf("  zen api error: %v", err)
	tracker.rec.RateLimited += rateLimited
	status := zenErrorStatus(err)
	// 请求轨迹: 直连路径的失败也要落"错误类别 + 消息", 否则面板只能看到裸状态码。
	if tr := traceFrom(r.Context()); tr != nil {
		tr.SetError(errClassForStatus(status), err.Error())
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": err.Error(), "type": "api_error"},
	})
	return status
}

// handleZenStreamChat 直连 zen 的**流式**回写, 带"空回包隐形换出口重试"。
//
// 用户规格(2026-09-24): 上游返回的任何错误(429 / 403 / 502 / 空回包)都必须在网关
// **内部消化**, 不得让 agent 看到; 网关自己换节点重来, 预算 5 分钟, 只有真实结果
// 才回给 agent。
//
// ★ 为什么这条路径必须单独实现(2026-09-24 23:18 用户实证):
//
//	直连 zen 路径(模型名形如 `zen/<model>`)根本不经过候选链 —— proxy.go 里
//	`resolveRouteChain` 只认配置过的别名(本机配置里只有 `auto-router`), 于是
//	落到 `routeModel(model)=="zen"` 分支。而候选链那条空流换站重试对它是无效的:
//	用户的 agent 发的正是 `zen/muse-spark-1.3-contributor-free`, 那次空回包把 502
//	直接甩给了 agent, agent 的工作被打断(症状从"卡死"变成"报错中断")。
//
//	用户要的"换节点"在候选链里是"换候选站", 在这条路径上**只能是换出口** ——
//	候选链只有一站时同样无站可换, 所以出口级重试是两条路都需要的底座。
//
// 重试的安全前提是**响应头尚未提交**(proxy_stream.go 的 lazyCommitWriter 保证空回包
// 时客户端一个字节都没收到)。已提交就改不了口 —— 再换一站会把第二次的内容接在
// 第一次已经发出去的流后面(用户明确警告过的顺序陷阱), 比不换更糟。
//
// 返回最终交付状态, 口径与 handleStreamResponseWithUsage 一致(<400 为成功)。
func handleZenStreamChat(w http.ResponseWriter, r *http.Request, params map[string]any, tracker *zenStatsTracker, usageFn func(map[string]any)) int {
	// 观测"响应头是否已提交" —— 复用候选链那条路径同一个探针。
	probe := &commitProbeWriter{ResponseWriter: w}
	w = probe

	// 空回包换出口的**独立时间预算**(emptyStreamRetryBudget, 用户拍板 5 分钟)。
	// 起点是**第一次遇到空回包**的时刻(惰性), 不与网络错 / 429 的重试窗口叠加
	// —— 那两类在 callZenAPI 内部各自有窗口。
	// 另有一个**次数**上限(emptyStreamMaxExitRotations), 理由见其注释。
	var deadline time.Time
	deadlineSet := false
	rotations := 0

	for {
		resp, rateLimited, err := callZenAPI(r.Context(), params, true)
		if err != nil {
			return zenWriteUpstreamError(w, r, tracker, rateLimited, err)
		}
		tracker.rec.RateLimited += rateLimited
		tracker.rec.Status = resp.StatusCode

		st := handleStreamResponseWithUsage(w, resp, usageFn)
		resp.Body.Close()
		if st < 400 {
			return st
		}

		// 放弃换出口时的统一收尾: 响应头若仍未提交, **必须显式写出真错误** ——
		// 否则 net/http 会替我们发一个 `200 + Content-Length: 0`, 客户端(agent 工具)
		// 收到的就是那条"空白回复", 等不到内容就卡死/中断(用户报的"空包中断"正是它)。
		// 已提交时是 no-op(错误帧已经写出去了, 再写正文会拼出破损响应)。
		giveUp := func() int {
			writeStreamEmptyFallback(probe, st)
			return st
		}

		// 走到这里 = 流处理器判定"整条流未交付任何有价值内容"(空回包)。
		// 只有**响应头未提交**才谈得上隐形重试 —— 此时客户端一个字节都没收到。
		if probe.committed.Load() {
			log.Printf("  zen: 流内空回包但响应头已提交(客户端已收到部分内容), 不再换出口, 交还 %d", st)
			return giveUp()
		}
		exit := reqExitKey(r.Context())
		if exit == "" {
			// 直连/兜底路径没有真实出口可换 —— "换出口"是空操作, 反复重试只是
			// 白打上游。与 429 分支同一口径(见 zen_call.go 的 cooled==0 分支)。
			log.Printf("  zen: 流内空回包但本次未记录到真实出口(reqExit 为空) — 无出口可换, 交还 %d", st)
			return giveUp()
		}
		if !deadlineSet {
			deadline = time.Now().Add(emptyStreamRetryBudget)
			deadlineSet = true
		}
		if rotations >= emptyStreamMaxExitRotations {
			log.Printf("  zen: 流内空回包, 换出口次数已达上限(%d), 交还 %d", emptyStreamMaxExitRotations, st)
			return giveUp()
		}
		if r.Context().Err() != nil {
			log.Printf("  zen: 流内空回包但客户端已断开, 停止换出口, 交还 %d", st)
			return giveUp()
		}
		if !time.Now().Before(deadline) {
			log.Printf("  zen: 流内空回包, 换出口预算(%s)已耗尽, 交还 %d", emptyStreamRetryBudget, st)
			return giveUp()
		}
		rotations++

		// 冷却刚用过的那个出口: 下一次 callZenAPI 选路时会跳过它。
		// 时长沿用出口级通用冷却(与拨号失败同口径, 见 zenDialGuardedPinned)。
		//
		// 注: 池子里已没有别的可用出口时, 选路会退化到兜底(直连)而不再写 reqExit,
		// 于是 reqExitKey 会停在上一轮的值上 —— 此时这一行冷却的是同一个出口(no-op)。
		// 不额外加"出口没变就放弃"的判断: 次数上限已经把这个退化情形收住了,
		// 而多加一条分支要配一套状态与用例, 收益不值。
		cooldownActualExit(r.Context(), emptyStreamExitCooldown)
		log.Printf("  zen: 流内空回包, 冷却出口 %s 并换出口重试 %d/%d(预算剩余 %s)",
			describeExitRaw(exit), rotations, emptyStreamMaxExitRotations,
			time.Until(deadline).Truncate(time.Second))
	}
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := params["max_tokens"].(float64); ok {
		maxTokens = int(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		maxTokens = int(mt)
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = normalizeRequestModel(m)
	}

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

// callClineAPIFailover wraps callClineAPI with cross-account failover
// (borrowed from okhsunrog/claude-proxy-rs's retry-and-rotate idea):
// retryable failures (429 / network / token refresh) re-pick the next
// account — pickAccount() already excludes cooled-down and expired
// accounts, so each retry naturally rotates — while non-retryable 4xx
// errors return immediately. Attempts are bounded by the pool size.
func callClineAPIFailover(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	total := len(poolSnapshot().Accounts)
	if total < 1 {
		total = 1
	}
	var (
		resp *http.Response
		acc  *Account
		err  error
	)
	for attempt := 0; attempt < total; attempt++ {
		// 客户端断开(或上层超时)后立即收手, 不再继续换账号重试。
		// 之前这里既没有 ctx 也没有取消检查, 客户端早就走了, 网关还在
		// 逐个账号把请求打完。
		if cerr := ctx.Err(); cerr != nil {
			return nil, acc, cerr
		}
		attemptCtx, cancelAttempt := clineAttemptCtx(ctx, stream)
		resp, acc, err = callClineAPI(attemptCtx, params, stream)
		if err == nil {
			if cancelAttempt != nil {
				// 正文由调用方读 —— 绝不能在此 cancel, 那会把调用方的正文读取掐断。
				// 绑到响应体关闭上(与 zenAttemptBody 同一手法)。
				resp.Body = &zenAttemptBody{ReadCloser: resp.Body, cancel: cancelAttempt}
			}
			return resp, acc, nil
		}
		if cancelAttempt != nil {
			cancelAttempt() // 错误路径: 立刻归还 timer, 不留悬空的 deadline
		}
		if !isRetryableUpstreamError(err) {
			return nil, acc, err
		}
		// 429 带 Retry-After 时小睡再换账号: 上游明确说"过 N 秒再来", 立刻换号
		// 重打只会把限流瞬间打满整个池。上限 5s, 且全程可被 ctx 取消打断。
		var wait time.Duration
		var ue *upstreamError
		if errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests && ue.RetryAfter > 0 {
			wait = ue.RetryAfter
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
		}
		if wait > 0 {
			log.Printf("  failover: 上游要求 %v 后重试, 等待再换账号", wait)
			select {
			case <-ctx.Done():
				return nil, acc, ctx.Err()
			case <-time.After(wait):
			}
		}
		log.Printf("  failover: attempt %d/%d failed (%v), rotating account", attempt+1, total, err)
	}
	return nil, acc, err
}

// clineAttemptCtx 给一次 cline 上游尝试准备 ctx。
//
// **非流式加总超时**(nonStreamUpstreamTimeout, 与 zen 侧同口径, 2026-09-24 审计 P1-3):
//
//	这条路径用的 getZenHTTPClient() 没有客户端级 Timeout, 而它的 https 侧路(h2)被
//	RegisterProtocol 旁路了主 transport 的 ResponseHeaderTimeout —— 剩下的时间防线
//	只有 h2 的 ReadIdleTimeout(30s)+PING(15s), 那只覆盖"对端完全不发帧"。上游
//	"响应头到了、正文停摆"(或持续吐心跳却不收尾)时, 调用方的
//	handleNonStreamResponseWithUsage → io.ReadAll 会一直等, handler 与 goroutine
//	长期占着, 客户端只能自己掐。
//
// **流式不加**: 长回答可以持续很久, 它靠 idleAbortReader 的空闲中断兜。
//
// 返回的 cancel 必须绑到**响应体关闭**上(见调用点的 zenAttemptBody), 不能在本次
// 尝试返回前调用 —— 正文是调用方读的, 提前 cancel 会把正文读取掐断。
func clineAttemptCtx(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		return ctx, nil
	}
	return context.WithTimeout(ctx, nonStreamUpstreamTimeout)
}

// isRetryableUpstreamError 判断 callClineAPI 返回的错误是否值得换账号重试。
//
// 优先级: 先看结构化错误里的 HTTP 状态码 —— callClineAPI 对一切非 200 响应都会
// 包装成 *upstreamError 并带上 Status。按状态码判定最稳: 429 限流与 5xx 重试,
// 其余(包括所有 4xx, 如 400 模型不存在 / 401 鉴权 / 403 地域限制)一律不重试,
// 否则这些"必然失败"的请求会被白白打满整个账号池的 failover 轮次。
//
// 字符串兜底不能删: 网络层错误(token 刷新失败、拨号失败)和早期的调用点返回的是
// 没有 Status 的裸 fmt.Errorf, 只能靠既有文案关键词识别。一刀切删掉会让网络抖动
// 被当成"不可重试"而直接 502。
func isRetryableUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *upstreamError
	if errors.As(err, &ue) && ue.Status != 0 {
		switch ue.Status {
		case http.StatusTooManyRequests, // 429 限流
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		}
		// 其余(含全部 4xx)不重试。
		return false
	}
	// 兜底: 无状态码的错误按文案关键词判断(网络错误 / token 刷新失败等)。
	s := err.Error()
	for _, mark := range []string{"429", "token failed", "token expired", "refresh failed", "network error", "upstream request", "upstream retry"} {
		if strings.Contains(s, mark) {
			return true
		}
	}
	return false
}

func callClineAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	acc := pickAccount()
	if acc == nil {
		return nil, nil, fmt.Errorf("no active accounts available: %s", describePoolStatus())
	}

	token, err := ensureAccountToken(acc)
	if err != nil {
		// Try other accounts
		return nil, nil, fmt.Errorf("account %s token failed: %w", acc.Email, err)
	}

	body := buildUpstreamBody(params, stream)
	sessionID, _ := body["session_id"].(string)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, acc, fmt.Errorf("marshal body: %w", err)
	}

	// newClineRequest 每次发送都重建请求对象。
	//
	// http.Request 的 Body 是一次性的: 首次 Do 之后 bytes.Reader 已经读到 EOF
	// 并被关闭。此前 401 分支刷新 token 后直接复用同一个 req 再 Do 一次, 于是
	// transport 报 "http: ContentLength=N with Body length 0" —— 也就是说
	// token 刷新成功之后的补救请求必然失败, 单账号池上直接表现成 500, 多账号池
	// 则被外层换账号掩盖过去。同库 providers_chat.go 踩过同一个坑并留了注释。
	//
	// 顺手带上 ctx: 客户端断开后请求要能被取消, 而不是继续把上游打完。
	newClineRequest := func(tok string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, err
		}
		req.Header = clineHeaders(tok, sessionID)
		return req, nil
	}

	req, err := newClineRequest(token)
	if err != nil {
		return nil, acc, fmt.Errorf("create request: %w", err)
	}

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
		truncateEmail(acc.Email), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

	resp, err := getZenHTTPClient().Do(req)
	if err != nil {
		// 网络错误：临时短冷却 5 分钟
		markAccountCooldown(acc, "network error: "+err.Error(), 5*time.Minute)
		return nil, acc, fmt.Errorf("upstream request: %w", err)
	}

	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if err := refreshAccountToken(acc); err == nil {
			// acc.AccessToken 由 refreshAccountToken 在 poolMu 内写入, 读取也走同一把锁。
			poolMu.Lock()
			token = acc.AccessToken
			poolMu.Unlock()
			req, err = newClineRequest(token)
			if err != nil {
				return nil, acc, fmt.Errorf("rebuild request: %w", err)
			}
			resp, err = getZenHTTPClient().Do(req)
			if err != nil {
				return nil, acc, fmt.Errorf("upstream retry: %w", err)
			}
			if resp.StatusCode == 401 {
				resp.Body.Close()
				// savePoolLocked 内部释放 poolMu: 共享字段在锁内取, 之后不再 Unlock。
				poolMu.Lock()
				email := acc.Email
				acc.Status = "expired"
				savePoolLocked()
				return nil, acc, fmt.Errorf("account %s token expired permanently", email)
			}
		} else {
			// savePoolLocked 内部释放 poolMu: 共享字段在锁内取, 之后不再 Unlock。
			poolMu.Lock()
			email := acc.Email
			acc.Status = "expired"
			savePoolLocked()
			return nil, acc, fmt.Errorf("account %s refresh failed: %w", email, err)
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, providerResponseMaxBytes+1))
		resp.Body.Close()
		// Mark account on cooldown on rate limits
		var retryAfter time.Duration
		if resp.StatusCode == 429 {
			reason := kit.Truncate(string(bodyBytes), 500)
			duration := parseInferenceCapDuration(string(bodyBytes))
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			retryAfter = duration
			markAccountCooldown(acc, "429: "+reason, duration)
			log.Printf("  account %s cooldown %v (reason: %s)", truncateEmail(acc.Email), duration, reason)
		}
		// 返回带类型的上游错误, 而不是裸 fmt.Errorf。
		//
		// 候选链是靠**错误类型**取状态码的(chainErrorStatusBody 只认
		// upstreamError / zenUpstreamError / providerError)。此前这里返回裸
		// error, 于是 cline 的 429/403/404 一律拿不到状态码 → 被
		// classifyCandidateFailure 当成"网络超时"只做 5 分钟短冷却, 并且
		// 回给客户端的响应码被压成 502, 真实原因(限流/下架/无权限)全部丢失。
		// 同目录 providers_chat.go 早就用 providerError 这么做了, 这里补上。
		return nil, acc, &upstreamError{
			Upstream:   upstreamCline,
			Status:     resp.StatusCode,
			Body:       kit.Truncate(string(bodyBytes), 500),
			RetryAfter: retryAfter,
		}
	}

	bumpUsage(acc)
	return resp, acc, nil
}

// accountUsageFn 构造账号 token 记账回调：从上游 usage 提取
// prompt_tokens + completion_tokens，计入该账号今日/累计消耗。
//
// take-last 语义(P2-24): 流式上游会把**同一份累计** usage 在多个 chunk 里
// 重复发送 —— stats 侧取最后一次(stats.go observeUsage 注释自认"重复调用取
// 最后一次"), 而 recordAccountTokens 是 `+=` 累加, 逐 chunk 直接入账会按
// chunk 数成倍放大 TokensToday/TokensTotal, 账户配额被虚假用量耗尽。
// 闭包因此记录"本请求已入账的上一次值", 每次只补差额: 重复发送同一份累计
// usage 时差额为 0(recordAccountTokens 对 <=0 直接忽略), 最终入账量恰等于
// 最后一次 usage 的值, 与 stats 的 take-last 对齐; 非流式只调用一次,
// 差额即全额, 行为与旧实现等价。
// (注: 本函数有 4 个调用点, 其中 anthropic/responses/adapters 在本包修复
// 清单之外的文件里, 无法改造成"收尾时 flush"的形态 —— 差额入账是等价且
// 调用点零改动的实现。)
//
// params 估算兜底(上游未返回 usage 时按入站请求估算)同理只在"本请求尚无
// 任何入账"时生效一次, 不再按 chunk 数重复兜底。
func accountUsageFn(acc *Account, params map[string]any) func(map[string]any) {
	var applied float64 // 本请求已入账的上一次 usage 合计(take-last 的差额基准)
	return func(u map[string]any) {
		var pt, ct float64
		if v, ok := u["prompt_tokens"].(float64); ok {
			pt = v
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			ct = v
		}
		cur := pt + ct
		if cur <= 0 {
			// 上游未返回 usage: 用入站请求估算兜底（与 zen 统计一致）——
			// 只兜底一次, 已入过账就跳过。
			if applied > 0 || params == nil {
				return
			}
			est := float64(estimateJSON(params))
			if est <= 0 {
				return
			}
			recordAccountTokens(acc, int64(est))
			applied = est
			return
		}
		// usage 是累计值: 只补与上次入账的差额(重复/回退的 chunk 差额 <=0,
		// recordAccountTokens 直接忽略)。
		if delta := int64(cur - applied); delta > 0 {
			recordAccountTokens(acc, delta)
			applied = cur
		}
	}
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}
