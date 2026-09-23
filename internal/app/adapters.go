package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"free-router/internal/providers"
)

// gateway assembles the provider layer: the Router seam plus concrete
// providers. cline/zen keep their battle-tested call paths in this package
// and are exposed through thin adapters; clinepass is fully owned by the
// providers package (key pool + cooldown + RawChat for protocol reuse).
//
// 接线现状(重要, 2026-09-16 审计 P1-4 后的显式声明):
//
//	Router.Chat / Router.ChatStream / Router.ListAllModels —— **目前无生产调用方**。
//	chat/completions 主路径仍直接调 callClineAPIFailover / callZenAPI(历史保留:
//	它们带着账号池轮换、冷却、出口池与 usage 记账, 改走 Router 需要整体回归);
//	Router 当前唯一的生产入口是 clinepass(经 getGateway().ClinePass +
//	paramsToChatRequest)。
//	Router.KindOf 是内部使用(Router.Provider 依赖它)。ListAllModels 目前仅被
//	测试覆盖 —— 面板的模型列表走各 provider 自己的目录接口。
//
//	因此: 新增功能请优先落在既有直调路径, 不要以为"注册进 Router 就等于接上了"。
//	若要把主路径整体切到 Router.Chat, 属于独立重构, 必须配套端到端回归。
type gateway struct {
	Router    *providers.Router
	ClinePass *providers.ClinePassProvider
}

var (
	gw     *gateway
	gwOnce sync.Once
)

func getGateway() *gateway {
	gwOnce.Do(func() {
		cp := providers.NewClinePassProvider()
		r := providers.NewRouter(classifyModel, "cline")
		r.Register("cline", clineAdapter{})
		r.Register("zen", zenAdapter{})
		r.Register("clinepass", cp)
		gw = &gateway{Router: r, ClinePass: cp}
	})
	return gw
}

// classifyModel extends routeModel with the cline-pass prefix. "reject"
// is handled by callers before dispatch (they write the 400), so it maps
// to "" here and the Router falls back to cline.
func classifyModel(model string) string {
	if strings.HasPrefix(strings.TrimSpace(model), "cline-pass/") {
		return "clinepass"
	}
	model = stripDisplayPrefix(model)
	switch routeModel(model) {
	case "zen":
		return "zen"
	case "reject":
		return ""
	default:
		return "cline"
	}
}

// clineAdapter exposes the existing Cline account-pool path as a Provider.
type clineAdapter struct{}

func (clineAdapter) Kind() string { return "cline" }

func (clineAdapter) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	params := chatRequestToParams(req)
	resp, acc, err := callClineAPIFailover(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if d, ok := body["data"].(map[string]any); ok {
		body = d
	}
	usage, _ := body["usage"].(map[string]any)
	if acc != nil && usage != nil {
		accountUsageFn(acc, params)(usage)
	}
	return &providers.ChatResponse{Body: body, Status: 200, Usage: usage}, nil
}

func (clineAdapter) ChatStream(ctx context.Context, req providers.ChatRequest, w io.Writer, onUsage func(map[string]any)) error {
	// 接口写的是 io.Writer, 那就必须真的支持任意 io.Writer —— 中继需要一个能
	// 写响应头/Flush 的 ResponseWriter, 缺了就用适配器补上, 而不是报错拒绝。
	// (审计 P2-9: 旧实现断言失败即返回错误, 等于接口在说谎; 而 clinepass 链式
	//  转发路径确实只拿得到 io.PipeWriter, 收窄签名会直接破坏首字节门。)
	params := chatRequestToParams(req)
	resp, _, err := callClineAPIFailover(ctx, params, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	handleStreamResponseWithUsage(streamResponseWriter(w), resp, onUsage)
	return nil
}

func (clineAdapter) ListModels() []providers.ModelInfo {
	out := []providers.ModelInfo{}
	for _, m := range getFreeModels() {
		out = append(out, providers.ModelInfo{
			ID: m.ID, Source: "cline", Cost: "free", Provider: "cline",
			Context: 200000, Output: 32768,
		})
	}
	return out
}

func (clineAdapter) HealthCheck(ctx context.Context) error {
	if pickAccount() == nil {
		return fmt.Errorf("no active accounts (%s)", describePoolStatus())
	}
	return nil
}

// zenAdapter exposes the opencode zen path as a Provider.
type zenAdapter struct{}

func (zenAdapter) Kind() string { return "zen" }

func (zenAdapter) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	params := chatRequestToParams(req)
	resp, _, err := callZenAPI(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if d, ok := body["data"].(map[string]any); ok {
		body = d
	}
	usage, _ := body["usage"].(map[string]any)
	return &providers.ChatResponse{Body: body, Status: 200, Usage: usage}, nil
}

func (zenAdapter) ChatStream(ctx context.Context, req providers.ChatRequest, w io.Writer, onUsage func(map[string]any)) error {
	// 同 clineAdapter: 任意 io.Writer 都支持(见 streamResponseWriter)。
	params := chatRequestToParams(req)
	resp, _, err := callZenAPI(ctx, params, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	handleStreamResponseWithUsage(streamResponseWriter(w), resp, onUsage)
	return nil
}

// writerResponseWriter 把任意 io.Writer 提升为 http.ResponseWriter, 供中继
// (它要写响应头并 Flush)在只有 io.Writer 的路径上复用 —— 典型场景是
// clinepass 链式转发: 它用 io.Pipe 做"首字节门", 拿到的是 *io.PipeWriter。
//
// 语义: 响应头与状态码只在内存里记录(不改变底层 writer 的行为); Flush 下传到底层
// writer(若它自己有 Flush/Flusher 则调用, 否则 no-op —— io.Pipe 是无缓冲的, 写完
// 即达对端, 不需要额外刷新)。
type writerResponseWriter struct {
	w      io.Writer
	header http.Header
	status int
}

func (a *writerResponseWriter) Header() http.Header {
	if a.header == nil {
		a.header = http.Header{}
	}
	return a.header
}

func (a *writerResponseWriter) Write(p []byte) (int, error) { return a.w.Write(p) }

func (a *writerResponseWriter) WriteHeader(status int) { a.status = status }

func (a *writerResponseWriter) Flush() {
	type flusher interface{ Flush() }
	type flusherErr interface{ Flush() error }
	switch f := a.w.(type) {
	case flusher:
		f.Flush()
	case flusherErr:
		_ = f.Flush()
	}
}

// streamResponseWriter 已经是 http.ResponseWriter 时原样返回(保住真实的
// Header/Flush 语义), 否则用 writerResponseWriter 包一层。返回的 writer 必然
// 实现 http.Flusher —— 中继靠这个类型断言决定要不要继续转发流。
func streamResponseWriter(w io.Writer) http.ResponseWriter {
	if rw, ok := w.(http.ResponseWriter); ok {
		if _, hasFlush := w.(http.Flusher); hasFlush {
			return rw
		}
		return &flushableResponseWriter{ResponseWriter: rw}
	}
	return &writerResponseWriter{w: w}
}

// flushableResponseWriter 给"是 ResponseWriter 但没有 Flush"的实现补一个 no-op
// Flush, 否则中继会因为断言失败直接放弃转发。
type flushableResponseWriter struct {
	http.ResponseWriter
}

func (f *flushableResponseWriter) Flush() {}

func (zenAdapter) ListModels() []providers.ModelInfo {
	out := []providers.ModelInfo{}
	for _, m := range zenModelList() {
		id, _ := m["id"].(string)
		ctxSize, _ := m["context"].(int)
		outSize, _ := m["output"].(int)
		out = append(out, providers.ModelInfo{
			ID: id, Source: "zen", Cost: "free", Provider: "zen",
			Context: ctxSize, Output: outSize,
		})
	}
	return out
}

func (zenAdapter) HealthCheck(ctx context.Context) error {
	cfg := getZenConfig()
	if !cfg.Enabled {
		return fmt.Errorf("zen disabled")
	}
	return nil
}

// chatRequestToParams rebuilds the map shape the existing call paths
// consume (buildUpstreamBody / buildZenBody read from map[string]any).
//
// 保真策略(审计 P2-7): 以 Extra 为基底 —— 它通常就是客户端的原始 params,
// 于是 messages 等复杂字段**不必再走一次 JSON 往返**(旧实现无条件
// json.Unmarshal(req.Messages), 大对话每请求白编解码一遍), 客户端传来的
// 数值也保持原样(不会被 int 截断)。只有 Extra 里没有的字段才用强类型字段补齐。
//
// 例外: model / stream 始终以强类型字段为准 —— 调用方在转换前已完成模型名规整
// 与路由决策, 不能被原始值覆盖。
func chatRequestToParams(req providers.ChatRequest) map[string]any {
	params := make(map[string]any, len(req.Extra)+6)
	for k, v := range req.Extra {
		params[k] = v
	}
	if req.Model != "" {
		params["model"] = req.Model
	}
	params["stream"] = req.Stream
	if _, ok := params["max_tokens"]; !ok {
		if _, alt := params["max_completion_tokens"]; !alt && req.MaxTokens > 0 {
			params["max_tokens"] = float64(req.MaxTokens)
		}
	}
	if _, ok := params["temperature"]; !ok && req.Temperature != 0 {
		params["temperature"] = req.Temperature
	}
	if _, ok := params["top_p"]; !ok && req.TopP != 0 {
		params["top_p"] = req.TopP
	}
	if _, ok := params["messages"]; !ok && len(req.Messages) > 0 {
		var msgs any
		if json.Unmarshal(req.Messages, &msgs) == nil {
			params["messages"] = msgs
		}
	}
	return params
}

// paramsToChatRequest converts a parsed client params map into the
// provider-agnostic ChatRequest (used by the clinepass handler paths).
func paramsToChatRequest(params map[string]any, model string, stream bool) providers.ChatRequest {
	req := providers.ChatRequest{Model: model, Stream: stream, Extra: params}
	if mt, ok := params["max_tokens"].(float64); ok {
		req.MaxTokens = int(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		req.MaxTokens = int(mt)
	}
	if msgs, ok := params["messages"]; ok {
		if b, err := json.Marshal(msgs); err == nil {
			req.Messages = b
		}
	}
	return req
}
