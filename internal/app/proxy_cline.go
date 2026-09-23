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

	resp, rateLimited, err := callZenAPI(r.Context(), params, isStream)
	if err != nil {
		log.Printf("  zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		status := zenErrorStatus(err)
		// 请求轨迹: 直连路径的失败也要落"错误类别 + 消息", 否则面板只能看到裸状态码。
		if tr := traceFrom(r.Context()); tr != nil {
			tr.SetError(errClassForStatus(status), err.Error())
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, status)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

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
		st := handleStreamResponseWithUsage(w, resp, usageFn)
		tracker.finish(st < 400, st)
		return
	}
	st := handleNonStreamResponseWithUsage(w, resp, usageFn)
	tracker.finish(st < 400, st)
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
		resp, acc, err = callClineAPI(ctx, params, stream)
		if err == nil {
			return resp, acc, nil
		}
		if !isRetryableUpstreamError(err) {
			return nil, acc, err
		}
		log.Printf("  failover: attempt %d/%d failed (%v), rotating account", attempt+1, total, err)
	}
	return nil, acc, err
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
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Mark account on cooldown on rate limits
		if resp.StatusCode == 429 {
			reason := kit.Truncate(string(bodyBytes), 500)
			duration := parseInferenceCapDuration(string(bodyBytes))
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
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
			Upstream: upstreamCline,
			Status:   resp.StatusCode,
			Body:     kit.Truncate(string(bodyBytes), 500),
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
