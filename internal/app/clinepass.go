package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"free-router/internal/kit"
	"free-router/internal/protocol"
	"free-router/internal/providers"
)

// init 把共享 HTTP 传输(HTTPClient/HTTPClientTimeout 共用同一指针)的基础拨号
// 挂上 SSRF 运行时防线: dialWithSSRFGuard 在包 outbound_url.go, kit 不能 import
// app, 接线只能落在 app 侧 —— 包 init 早于 main, 对之后所有经 kit 客户端的出站
// 请求生效(配置期校验之外, 堵住 DNS rebinding 到链路本地/云元数据的窗口)。
func init() {
	kit.HTTPTransport.DialContext = dialWithSSRFGuard(
		(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
}

// ClinePass handler paths: cline-pass/ prefixed models route through the
// ClinePass key pool. The upstream speaks OpenAI chat format, so the
// existing stream converters (Anthropic events, Responses events) are
// reused directly over the raw upstream response.

// callClinePassChain 把 ClinePass provider 包成候选链要的 *http.Response 形态。
//
// 非流式: 上游 JSON 直接合成响应。
// 流式: 用管道转发 provider 写出的 SSE, 但**先等首字节**再返回 —— 这样"首字节
// 之前失败仍可换站"的 failover 语义与 zen/cline 一致(拿到 200+首包后才算命中)。
func callClinePassChain(ctx context.Context, params map[string]any, stream bool) (*http.Response, error) {
	cp := clinePassProvider()
	if cp == nil {
		return nil, fmt.Errorf("clinepass provider is not initialised")
	}
	model, _ := params["model"].(string)
	req := paramsToChatRequest(params, model, stream)

	if !stream {
		resp, err := cp.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("clinepass: encode response: %w", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
		}, nil
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := cp.ChatStream(ctx, req, pw, nil)
		_ = pw.CloseWithError(err)
		errCh <- err
	}()

	// 首字节门: 拿到第一段数据前失败 → 返回错误让链路换下一站。
	buf := make([]byte, 4096)
	n, err := pr.Read(buf)
	if n == 0 {
		select {
		case e := <-errCh:
			if e != nil {
				return nil, e
			}
		default:
		}
		if err != nil {
			return nil, fmt.Errorf("clinepass stream: %w", err)
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &pipeReadCloser{r: io.MultiReader(bytes.NewReader(buf[:n]), pr), pr: pr},
	}, nil
}

// pipeReadCloser 把「已读出的首段缓冲 + io.Pipe 读端」拼成一个有效 Close 的
// ReadCloser。此前外层是 NopCloser —— Close() 是空操作, pr 永远没人关:
// 上层 relay 停止读取后 pw.Write 同步永久阻塞(ctx 取消也解不开, 阻塞在 write
// 而非 select), ChatStream 协程卡死, 其内部 defer resp.Body.Close() 不执行,
// 每次断流泄漏 1 个 goroutine + 1 条上游连接。Close 时关掉 pr, 写端收到
// ErrClosedPipe 自然退出, goroutine 走 pw.CloseWithError + errCh(缓冲 1) 收尾。
type pipeReadCloser struct {
	r  io.Reader
	pr *io.PipeReader
}

func (c *pipeReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *pipeReadCloser) Close() error               { return c.pr.Close() }

// clinePassReady ClinePass 订阅池是否至少有一个可用 key(active)。
// 候选链用它判断"ClinePass 这一站现在能不能打", 全部冷却时直接跳过而不是白撞一次。
func clinePassReady() bool {
	cp := clinePassProvider()
	if cp == nil {
		return false
	}
	for _, st := range cp.KeyStatuses() {
		if s, _ := st["status"].(string); s == "active" {
			return true
		}
	}
	return false
}

// clinePassModelByID 在 ClinePass 订阅目录里查模型(候选链校验/上下文长度用)。
func clinePassModelByID(id string) (providers.ModelInfo, bool) {
	cp := clinePassProvider()
	if cp == nil {
		return providers.ModelInfo{}, false
	}
	for _, m := range cp.ListModels() {
		if m.ID == id {
			return m, true
		}
	}
	return providers.ModelInfo{}, false
}

func clinePassProvider() *providers.ClinePassProvider {
	return getGateway().ClinePass
}

// reloadClinepassKeys 供 .clinepass-keys.json 被本进程之外改写(面板配置导入)后
// 刷新内存 key 池 —— 不刷新的话, 下一次面板增删 key 的 p.save() 会用旧内存
// 整文件覆盖刚导入的内容(静默回退)。
func reloadClinepassKeys() {
	if cp := clinePassProvider(); cp != nil {
		cp.Reload()
	}
}

// handleClinePassChat serves POST /v1/chat/completions with cline-pass/ models.
func handleClinePassChat(w http.ResponseWriter, r *http.Request, params map[string]any, isStream bool) {
	model, _ := params["model"].(string)
	cp := clinePassProvider()
	req := paramsToChatRequest(params, model, isStream)
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		setCORSOrigin(w)
		w.WriteHeader(http.StatusOK)
		// usage 回调接入统计(P2-8): 否则 clinepass 流式路径 CompletionTokens 恒 0。
		if err := cp.ChatStream(r.Context(), req, w, tracker.observeUsage); err != nil {
			log.Printf("  clinepass stream error: %v", err)
			tracker.finish(false, http.StatusBadGateway)
			return
		}
		tracker.finish(true, http.StatusOK)
		return
	}

	resp, err := cp.Chat(r.Context(), req)
	if err != nil {
		log.Printf("  clinepass api error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	// 非流式同样要把上游 usage 计入统计(P2-8), take-last 语义在 observeUsage 内。
	tracker.observeUsage(resp.Usage)
	tracker.finish(true, http.StatusOK)
	writeJSON(w, http.StatusOK, resp.Body)
}

// handleClinePassAnthropic serves POST /v1/messages with cline-pass/ models.
func handleClinePassAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cp := clinePassProvider()
	isStream := req.Stream
	creq := paramsToChatRequest(openAIReq, req.Model, isStream)
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        req.Model,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	up, err := cp.RawChat(r.Context(), creq, isStream)
	if err != nil {
		log.Printf("  clinepass anthropic error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	defer up.Body.Close()
	tracker.rec.Status = up.StatusCode

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		setCORSOrigin(w)
		w.WriteHeader(http.StatusOK)
		st := handleAnthropicStreamWithUsage(w, up, req.Model, toolSchemas, tracker.observeUsage)
		tracker.finish(st < 400, st)
		return
	}

	var raw map[string]any
	// 上游响应体封顶(2026-09-24 审计 P0-2: 出口是第三方节点, 响应字节不可信; 见 kit.DecodeJSONLimit)
	if err := kit.DecodeJSONLimit(up.Body, &raw, providerResponseMaxBytes); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	if d, ok := raw["data"].(map[string]any); ok {
		raw = d
	}
	chatOut := protocol.NormalizeOpenAIChunk(raw)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	// 非流式: 把上游 usage 计入统计(P2-8)。
	if u, ok := raw["usage"].(map[string]any); ok {
		tracker.observeUsage(u)
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, up.StatusCode)
}

// handleClinePassResponses serves POST /v1/responses with cline-pass/ models.
func handleClinePassResponses(w http.ResponseWriter, r *http.Request, params, chat map[string]any, isStream bool) {
	cp := clinePassProvider()
	model, _ := chat["model"].(string)
	creq := paramsToChatRequest(chat, model, isStream)
	tracker := newZenStatsTrackerCtx(r.Context(), zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(chat),
	})

	up, err := cp.RawChat(r.Context(), creq, isStream)
	if err != nil {
		log.Printf("  clinepass responses error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	defer up.Body.Close()
	tracker.rec.Status = up.StatusCode

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		setCORSOrigin(w)
		w.WriteHeader(http.StatusOK)
		st := chatStreamToResponses(w, up, tracker.observeUsage)
		tracker.finish(st < 400, st)
		return
	}

	var raw map[string]any
	// 上游响应体封顶(2026-09-24 审计 P0-2: 出口是第三方节点, 响应字节不可信; 见 kit.DecodeJSONLimit)
	if err := kit.DecodeJSONLimit(up.Body, &raw, providerResponseMaxBytes); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	if d, ok := raw["data"].(map[string]any); ok {
		raw = d
	}
	// 非流式: 把上游 usage 计入统计(P2-8)。
	if u, ok := raw["usage"].(map[string]any); ok {
		tracker.observeUsage(u)
	}
	writeJSON(w, http.StatusOK, chatToResponses(raw))
	tracker.finish(true, up.StatusCode)
}

// nowMillis local alias keeps handler bodies tidy.
func nowMillis() int64 { return protocol.NowMillis() }

// registerClinePassAdminRoutes wires key management into the admin API.
func registerClinePassAdminRoutes(mux *http.ServeMux) {
	cp := clinePassProvider()
	mux.HandleFunc("/admin/api/clinepass/keys", adminAuth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
		case "POST":
			var body struct {
				Key string `json:"key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body must be {\"key\": \"...\"}"})
				return
			}
			cp.AddKey(body.Key)
			writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
		default:
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		}
	}))
	mux.HandleFunc("/admin/api/clinepass/keys/delete", adminAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
			return
		}
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body must be {\"key\": \"<masked>\"}"})
			return
		}
		cp.RemoveKey(body.Key)
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
	}))
	mux.HandleFunc("/admin/api/clinepass/models", adminAuth(func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": clinePassProvider().ListModels()}})
	}))
	_ = fmt.Sprint() // keep fmt import if handlers change
}
