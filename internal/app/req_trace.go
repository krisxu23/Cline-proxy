package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 请求轨迹 (reqTrace): 一次客户端请求内部发生了什么
//
// 参照 OmniRoute 的 call_logs(一行摘要答完"谁/哪个上游/多快/多少 token/为何失败")
// 与 decisionTrace(每次路由决策一条轨迹, 只记元数据不记 prompt)。
//
// 设计要点:
//   - 由 requestLogMiddleware 注入 context, 链路上任何一层都能回填, 结束前由
//     中间件一次性写入请求日志 —— 调用方不需要知道谁在读。
//   - 只存"排查必需"的元数据: 上游名、实际模型、尝试/跳过轨迹、错误类别、
//     token 用量、TTFT。**不含 prompt / 响应正文 / 请求头**, 避免把日志变成
//    数据泄露面(与 OmniRoute 的 decisionTrace 契约一致)。
//   - 并发安全: 一条请求可能有多段写(candidate 循环 + 流式回调), 内部加锁。
// ============================================================================

// reqTrace 单次请求的链路轨迹。
type reqTrace struct {
	mu sync.Mutex

	RequestID string
	Upstream  string // zen / cline / clinepass / provider/<name>
	Resolved  string // 实际发往上游的模型名
	Protocol  string // openai | anthropic | responses
	Stream    bool
	Attempts  int
	Skipped   []string // "候选=原因" 列表, 供日志与面板详情展示
	ErrClass  string
	ErrMsg    string

	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CacheTokens      int

	// tokenReported 上游是否真的报过 usage。用于区分"报了 0"与"没报"
	// (OmniRoute 的 NULL vs 0 纪律): 没报时面板显示"未上报", 而不是假的 0。
	tokenReported bool
}

type reqTraceCtxKey struct{}

// ctxKeyReqTrace 中间件注入、链路回填的轨迹指针。
var ctxKeyReqTrace = reqTraceCtxKey{}

// newRequestID 生成请求 id。优先沿用客户端传的 X-Request-Id(便于跨系统串联),
// 缺失时生成 req_<16hex>。
func newRequestID(clientValue string) string {
	if v := strings.TrimSpace(clientValue); v != "" && len(v) <= 64 {
		// 只保留可打印安全字符, 防止把日志字段玩坏。
		safe := true
		for i := 0; i < len(v); i++ {
			c := v[i]
			if c <= ' ' || c > '~' {
				safe = false
				break
			}
		}
		if safe {
			return v
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req_" + time.Now().Format("150405.000000")
	}
	return "req_" + hex.EncodeToString(b)
}

// traceFrom 取出当前请求的轨迹; 没有(未过中间件)时返回 nil。
// 所有回填都用 `if tr := traceFrom(ctx); tr != nil` 保护, 便于单测直接调用。
func traceFrom(ctx context.Context) *reqTrace {
	if ctx == nil {
		return nil
	}
	if tr, ok := ctx.Value(ctxKeyReqTrace).(*reqTrace); ok {
		return tr
	}
	return nil
}

// reqIDFrom 取当前请求 id, 缺失时返回空串。
func reqIDFrom(ctx context.Context) string {
	if tr := traceFrom(ctx); tr != nil {
		return tr.RequestID
	}
	return ""
}

// withReqTrace 把轨迹注入 context(中间件使用)。
func withReqTrace(ctx context.Context, tr *reqTrace) context.Context {
	return context.WithValue(ctx, ctxKeyReqTrace, tr)
}

// ---- 回填接口(全部并发安全, 全部容忍 nil) ----

// SetUpstream 记录实际命中的上游与发往它的模型名。
func (t *reqTrace) SetUpstream(upstream, resolvedModel string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if upstream != "" {
		t.Upstream = upstream
	}
	if resolvedModel != "" {
		t.Resolved = resolvedModel
	}
}

// SetProtocol 记录客户端入口协议(openai/anthropic/responses)与是否流式。
func (t *reqTrace) SetProtocol(protocol string, stream bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if protocol != "" {
		t.Protocol = protocol
	}
	if stream {
		t.Stream = true
	}
}

// AddAttempt 记录一次真实的候选尝试。
func (t *reqTrace) AddAttempt() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.Attempts++
	t.mu.Unlock()
}

// AddSkip 记录一个被跳过的候选及原因(冷却/永久剔除/日限额…)。
func (t *reqTrace) AddSkip(what, why string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Skipped) >= 16 { // 上限: 避免异常链把日志撑爆
		return
	}
	if why == "" {
		why = "跳过"
	}
	t.Skipped = append(t.Skipped, what+"="+why)
}

// SetError 记录最终失败类别与消息(成功时不设置)。
func (t *reqTrace) SetError(class, msg string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if class != "" {
		t.ErrClass = class
	}
	if msg != "" {
		t.ErrMsg = truncateRunes(msg, 300)
	}
}

// SetTokens 记录归一后的 token 用量(来自上游 usage)。
func (t *reqTrace) SetTokens(prompt, completion, reasoning, cache int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tokenReported = true
	if prompt > 0 {
		t.PromptTokens = prompt
	}
	if completion > 0 {
		t.CompletionTokens = completion
	}
	t.ReasoningTokens = reasoning
	t.CacheTokens = cache
}

// ObserveUsage 直接吃上游 usage 对象(OpenAI / Anthropic / Responses 三种字段名)。
// 链路里把它包在既有 tracker.observeUsage 外面即可, 无需改动各上游实现。
func (t *reqTrace) ObserveUsage(u map[string]any) {
	if t == nil || u == nil {
		return
	}
	p, c, r, cache, ok := parseUsageFields(u)
	if !ok {
		return
	}
	t.SetTokens(p, c, r, cache)
}

// parseUsageFields 把各协议的 usage 归一到 (prompt, completion, reasoning, cache)。
//
// 字段名差异覆盖:
//   - OpenAI chat      : prompt_tokens / completion_tokens / prompt_tokens_details.cached_tokens /
//     completion_tokens_details.reasoning_tokens
//   - OpenAI Responses : input_tokens / output_tokens / input_tokens_details.cached_tokens /
//     output_tokens_details.reasoning_tokens
//   - Anthropic        : input_tokens / output_tokens / cache_read_input_tokens / cache_creation_input_tokens
//
// 与 OmniRoute 的纪律一致: cache 读写不重复计入 prompt(Anthropic 的 input_tokens
// 本身不含 cache, 需要相加才是完整上下文)。
func parseUsageFields(u map[string]any) (prompt, completion, reasoning, cache int, ok bool) {
	num := func(m map[string]any, keys ...string) (int, bool) {
		for _, k := range keys {
			if v, exists := m[k]; exists {
				if f, isNum := v.(float64); isNum {
					return int(f), true
				}
				if i, isInt := v.(int); isInt {
					return i, true
				}
			}
		}
		return 0, false
	}

	p, gotP := num(u, "prompt_tokens", "input_tokens")
	c, gotC := num(u, "completion_tokens", "output_tokens")
	if !gotP && !gotC {
		return 0, 0, 0, 0, false
	}

	if d, isMap := u["prompt_tokens_details"].(map[string]any); isMap {
		if v, has := num(d, "cached_tokens"); has {
			cache += v
		}
	}
	if d, isMap := u["input_tokens_details"].(map[string]any); isMap {
		if v, has := num(d, "cached_tokens"); has {
			cache += v
		}
	}
	if v, has := num(u, "cache_read_input_tokens"); has {
		cache += v
	}
	if v, has := num(u, "cache_creation_input_tokens"); has {
		cache += v
	}
	if d, isMap := u["completion_tokens_details"].(map[string]any); isMap {
		if v, has := num(d, "reasoning_tokens"); has {
			reasoning = v
		}
	}
	if d, isMap := u["output_tokens_details"].(map[string]any); isMap {
		if v, has := num(d, "reasoning_tokens"); has {
			reasoning = v
		}
	}
	return p, c, reasoning, cache, true
}

// reqTraceSnapshot 轨迹的只读快照(写日志时一次性取, 避免持锁拼装)。
type reqTraceSnapshot struct {
	RequestID        string
	Upstream         string
	Resolved         string
	Protocol         string
	Stream           bool
	Attempts         int
	Skipped          []string
	ErrClass         string
	ErrMsg           string
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CacheTokens      int
	UsageReported    bool
}

// snapshot 取只读快照。
func (t *reqTrace) snapshot() reqTraceSnapshot {
	if t == nil {
		return reqTraceSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return reqTraceSnapshot{
		RequestID:        t.RequestID,
		Upstream:         t.Upstream,
		Resolved:         t.Resolved,
		Protocol:         t.Protocol,
		Stream:           t.Stream,
		Attempts:         t.Attempts,
		Skipped:          append([]string(nil), t.Skipped...),
		ErrClass:         t.ErrClass,
		ErrMsg:           t.ErrMsg,
		PromptTokens:     t.PromptTokens,
		CompletionTokens: t.CompletionTokens,
		ReasoningTokens:  t.ReasoningTokens,
		CacheTokens:      t.CacheTokens,
		UsageReported:    t.tokenReported,
	}
}

// truncateRunes 按字符截断(避免把多字节字符切坏)。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ============================================================================
// 路由决策轨迹 (P2-16): "为什么选了这一站 / 为什么跳过其它"
//
// 纯内存、只记元数据、TTL + LRU 上限, 与 OmniRoute 的 decisionTrace 同构:
//   - TTL: 30 分钟(排查窗口足够, 内存不长期驻留)
//   - LRU: 最近 2000 条请求
//   - 不记 prompt / body / header
// ============================================================================

const (
	decisionTraceTTL     = 30 * time.Minute
	decisionTraceMaxKeep = 2000
)

type candidateDecision struct {
	Candidate string `json:"candidate"` // upstream|model
	Decision  string `json:"decision"`  // tried | skipped | not_reached
	Reason    string `json:"reason,omitempty"`
	Status    int    `json:"status,omitempty"`
	ErrClass  string `json:"errClass,omitempty"`
}

type routeDecision struct {
	RequestID  string              `json:"requestId"`
	Time       time.Time           `json:"time"`
	Requested  string              `json:"requested"`
	Winner     string              `json:"winner,omitempty"`
	Candidates []candidateDecision `json:"candidates"`
	Finished   bool                `json:"finished"`
}

var (
	decisionMu    sync.Mutex
	decisionRing  []*routeDecision // 按完成顺序追加, 超限从头淘汰
	decisionIndex = map[string]*routeDecision{}
)

// decisionTraceStart 开始一条决策轨迹(链式调度入口调用)。
func decisionTraceStart(reqID, requested string) *routeDecision {
	if reqID == "" {
		return nil
	}
	d := &routeDecision{RequestID: reqID, Time: time.Now(), Requested: requested}
	decisionMu.Lock()
	pruneDecisionRingLocked()
	decisionIndex[reqID] = d
	decisionRing = append(decisionRing, d)
	for len(decisionRing) > decisionTraceMaxKeep {
		old := decisionRing[0]
		decisionRing = decisionRing[1:]
		if decisionIndex[old.RequestID] == old {
			delete(decisionIndex, old.RequestID)
		}
	}
	decisionMu.Unlock()
	return d
}

// addCandidate 记录一个候选的处置(调用方需持有 d 的独占权, 链式调度是串行的)。
func (d *routeDecision) addCandidate(cand, decision, reason string, status int, errClass string) {
	if d == nil {
		return
	}
	decisionMu.Lock()
	defer decisionMu.Unlock()
	d.Candidates = append(d.Candidates, candidateDecision{
		Candidate: cand, Decision: decision, Reason: reason, Status: status, ErrClass: errClass,
	})
}

func (d *routeDecision) setWinner(winner string) {
	if d == nil {
		return
	}
	decisionMu.Lock()
	d.Winner = winner
	d.Finished = true
	decisionMu.Unlock()
}

func (d *routeDecision) finish() {
	if d == nil {
		return
	}
	decisionMu.Lock()
	d.Finished = true
	decisionMu.Unlock()
}

// pruneDecisionRingLocked 丢弃过期条目(TTL)。调用方需持有 decisionMu。
func pruneDecisionRingLocked() {
	cutoff := time.Now().Add(-decisionTraceTTL)
	kept := decisionRing[:0]
	for _, r := range decisionRing {
		if r.Time.After(cutoff) {
			kept = append(kept, r)
			continue
		}
		if decisionIndex[r.RequestID] == r {
			delete(decisionIndex, r.RequestID)
		}
	}
	decisionRing = kept
}

// decisionTraceFor 按请求 id 取轨迹(面板详情用)。返回值是拷贝, 调用方可安全读取。
func decisionTraceFor(reqID string) *routeDecision {
	if reqID == "" {
		return nil
	}
	decisionMu.Lock()
	defer decisionMu.Unlock()
	d := decisionIndex[reqID]
	if d == nil {
		return nil
	}
	if time.Since(d.Time) > decisionTraceTTL {
		delete(decisionIndex, reqID)
		return nil
	}
	cp := *d
	cp.Candidates = append([]candidateDecision(nil), d.Candidates...)
	return &cp
}

// decisionTraceCount 当前保留的轨迹条数(健康面板/测试用)。
func decisionTraceCount() int {
	decisionMu.Lock()
	defer decisionMu.Unlock()
	return len(decisionRing)
}

// decisionTraceReset 清空轨迹(测试用)。
func decisionTraceReset() {
	decisionMu.Lock()
	decisionRing = nil
	decisionIndex = map[string]*routeDecision{}
	decisionMu.Unlock()
}
