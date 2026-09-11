package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

const (
	providerAttemptTimeout   = 180 * time.Second
	providerResponseMaxBytes = 64 << 20
	// providerExitRetries 网络错误时换出口重试次数(共 1 + N 次拨号)。
	providerExitRetries = 2
	// providerExitCooldown 网络错误后该出口的冷却时长。短冷却即可:
	// 目的是让轮询跳过"对这个上游不通"的节点, 而不是长期禁用可用节点。
	providerExitCooldown = 2 * time.Minute
)

// rotateProviderExit 冷却刚失败的那个出口, 并记录一次换出口重试。
// 直连模式下池内没有可选出口, 调用方不会走到这里。
func rotateProviderExit(provider, reason string, attempt int) {
	if idx := lastZenProxyIdx(); idx >= 0 {
		cooldownZenProxy(idx, providerExitCooldown)
	}
	log.Printf("  providers: %s %s, retry %d/%d on the next exit", provider, reason, attempt, providerExitRetries)
}

// isExitRegionRejected 上游按"出口所在地区"拒绝服务。
//
// 实测: 从不受支持的地区直连 Google, 目录与对话接口都回
// 400 FAILED_PRECONDITION "User location is not supported for the API use."。
// 这类失败与"这个节点到该上游不通"同源 —— 换一个地区的出口即可成功,
// 因此要和网络错误一样触发换出口, 而不是把 400 原样透传给调用方。
func isExitRegionRejected(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "location is not supported") ||
		strings.Contains(low, "user location")
}

// providerExitClient 所有通用 Provider 的上游请求都经此客户端发出。
// 它复用 zen 上游的传输层, 因此与 cline 池 / opencode 共用同一条出口链路:
// 出口模式(直连 / 节点) + 代理策略 + 节点连通性/冷却/地区能力全部一致生效。
// 早期实现给 Provider 单独配了直连客户端, 于是节点池对它完全不生效 ——
// 这正是「节点测试绿色、Provider 却 502/超时」的原因。
func providerExitClient() *http.Client {
	return getZenHTTPClient()
}

// providerError 上游错误(状态码 + 截断响应体)。
type providerError struct {
	Provider string
	Status   int
	Body     string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("provider %s: HTTP %d: %s", e.Provider, e.Status, kit.Truncate(e.Body, 500))
}

// providerErrorStatus 上游 4xx 原样透传, 其余(网络错误/5xx)按 502。
func providerErrorStatus(err error) int {
	if pe, ok := err.(*providerError); ok {
		if pe.Status >= 400 && pe.Status < 500 {
			return pe.Status
		}
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// parseProviderModel 解析 "provider:model" 前缀; provider 必须已在配置中声明。
func parseProviderModel(model string) (string, string, bool) {
	m := strings.TrimSpace(model)
	i := strings.Index(m, ":")
	if i <= 0 {
		return "", "", false
	}
	name, rest := m[:i], m[i+1:]
	if rest == "" || providerByName(name) == nil {
		return "", "", false
	}
	return name, rest, true
}

// sseTapReader 边读边按行回调(用于流式提取 Gemini 签名)。
type sseTapReader struct {
	rc      io.ReadCloser
	onLine  func(string)
	pending []byte
}

func (t *sseTapReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.pending = append(t.pending, p[:n]...)
		for {
			i := bytes.IndexByte(t.pending, '\n')
			if i < 0 {
				break
			}
			line := string(t.pending[:i])
			t.pending = t.pending[i+1:]
			t.onLine(line)
		}
	}
	return n, err
}

func (t *sseTapReader) Close() error {
	if len(t.pending) > 0 {
		t.onLine(string(t.pending))
		t.pending = nil
	}
	return t.rc.Close()
}

// Chat 转发 OpenAI 兼容请求到该 provider; 流式响应按 SSE 原样透传。
func (p *modelProvider) Chat(ctx context.Context, params map[string]any, stream bool) (*http.Response, error) {
	cfg, _ := providerConfigFor(p.name)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("provider %s is not configured", p.name)
	}
	if model, _ := params["model"].(string); model != "" {
		if rest, ok := strings.CutPrefix(model, p.name+":"); ok && rest != "" {
			params["model"] = rest
		}
	}
	model, _ := params["model"].(string)

	needsSig := providerNeedsThoughtSignatures(p)
	if needsSig {
		injectThoughtSignatures(params, p.sigCache)
	}
	client := providerExitClient()
	// 把模型写进请求上下文: 拨号层据此为地区受限模型挑选已验证的节点出口,
	// 与 opencode 渠道同一套选路规则。
	ctx = context.WithValue(ctx, ctxKeyZenModel, p.name+":"+model)
	if !stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, providerAttemptTimeout)
		defer cancel()
	}

	// Gemini 会拒绝缓存里失效的签名(400)。此时用跳过哨兵重放一次,
	// 否则同一段会话会一直失败到该缓存项被淘汰为止。
	attempts := 1
	if needsSig {
		attempts = 2
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		payload, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal provider body: %w", err)
		}
		origin := localOrigin()
		// 新请求要重建 body, 并重新取一次出口: 因此逐次构造而不是复用 req。
		send := func() (*http.Response, error) {
			req, rerr := http.NewRequestWithContext(ctx, "POST", cfg.chatEndpoint(), bytes.NewReader(payload))
			if rerr != nil {
				return nil, rerr
			}
			req.Header.Set("Content-Type", "application/json")
			cfg.applyAuth(req.URL.String(), req.Header.Set)
			for k, spec := range cfg.Headers {
				if v := cfg.resolveHeader(spec, origin); v != "" {
					req.Header.Set(k, v)
				}
			}
			return client.Do(req)
		}

		// 两种失败都可能只是"这个出口不行", 而不是"这个上游不行", 因此统一按出口轮换:
		//   - 网络错误: 所选出口到该上游不通;
		//   - 地区拒绝: 出口所在地区被上游拒服务(Google 的 location not supported)。
		// 与 zen 渠道同一套自愈逻辑 —— 否则池里一个不合适的节点会被反复选中。
		var (
			resp *http.Response
			body []byte
		)
		for exitRetries := 0; ; {
			r, sendErr := send()
			if sendErr != nil {
				// 直连模式没有第二个出口可换, 重复拨号只是白等一轮超时。
				if exitModeDirectNow() || exitRetries >= providerExitRetries {
					return nil, sendErr
				}
				exitRetries++
				rotateProviderExit(p.name, fmt.Sprintf("network error (%v)", sendErr), exitRetries)
				continue
			}
			resp = r
			if resp.StatusCode == http.StatusOK {
				return p.finishResponse(resp, stream, needsSig)
			}
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if exitRetries < providerExitRetries && !exitModeDirectNow() &&
				isExitRegionRejected(resp.StatusCode, body) {
				exitRetries++
				rotateProviderExit(p.name, "exit region rejected: "+kit.Truncate(string(body), 160), exitRetries)
				continue
			}
			break
		}
		if needsSig && attempt < attempts && isMissingThoughtSignatureError(resp.StatusCode, string(body)) {
			log.Printf("  providers: %s rejected a thought-signature, replaying with the skip sentinel", p.name)
			markAllSignaturesSkipped(params)
			continue
		}
		// Gemini 的 OpenAI 兼容层既接受裸模型名, 也接受带 models/ 前缀的名字。
		// 裸名被拒时补一次前缀形式, 免得用户为了一个命名约定去翻文档。
		if isGoogleProvider(cfg) && resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(model, "models/") {
			log.Printf("  providers: %s rejected model %q as-is, retrying with the models/ prefix", p.name, model)
			params["model"] = "models/" + model
			model = "models/" + model
			attempts++
			continue
		}
		p.recordRejection(model, resp.StatusCode, body)
		return nil, &providerError{Provider: p.name, Status: resp.StatusCode, Body: string(body)}
	}
	return nil, fmt.Errorf("provider %s: no attempt completed", p.name)
}

// finishResponse 收尾成功响应: 非流式必须在超时上下文内读完整个响应体,
// 流式则为 Gemini 挂上签名提取。
func (p *modelProvider) finishResponse(resp *http.Response, stream, needsSig bool) (*http.Response, error) {
	if !stream {
		// 返回即触发调用方的 defer cancel, 而请求上下文控制整个响应生命周期,
		// 未读完的 body 会被 "context canceled" 提前截断(客户端表现为 500 parse_error)。
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, providerResponseMaxBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(body) > providerResponseMaxBytes {
			return nil, fmt.Errorf("provider %s: response exceeds %d bytes", p.name, providerResponseMaxBytes)
		}
		if needsSig {
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				rememberSignaturesFromPayload(parsed, p.sigCache)
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	if needsSig {
		ext := newSignatureStreamExtractor(p.sigCache)
		resp.Body = &sseTapReader{rc: resp.Body, onLine: func(line string) { ext.push(line) }}
	}
	return resp, nil
}

// recordRejection 记录永久拒绝与"无免费层"到该 provider 的拒绝集合。
func (p *modelProvider) recordRejection(model string, status int, body []byte) {
	var payload map[string]any
	json.Unmarshal(body, &payload)
	reason := permanentRejectionReason(status, payload)
	if reason == "" && status == http.StatusTooManyRequests {
		if qf := parseQuotaFailure(payload); qf != nil && qf.NoFreeTier {
			reason = "no free tier"
		}
	}
	if reason == "" || strings.TrimSpace(model) == "" {
		return
	}
	// 写时复制: isFree/freeModelIDs 在解锁后读取该 map 的内容, 就地插入
	// 会与之并发触发 fatal 的 map 读写竞争, 因此整体替换而不是原地增删。
	p.mu.Lock()
	next := make(map[string]string, len(p.rejected)+1)
	for k, v := range p.rejected {
		next[k] = v
	}
	next[model] = reason
	p.rejected = next
	p.mu.Unlock()
}

// handleProviderChat "provider:model" 前缀直选分支。
func handleProviderChat(w http.ResponseWriter, r *http.Request, params map[string]any, name string) {
	p := providerByName(name)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q is not configured", name), "type": "api_error"},
		})
		return
	}
	cfg, _ := providerConfigFor(name)
	if cfg.APIKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q has no api key", name), "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	pm := strings.TrimPrefix(model, name+":")

	if cfg.Catalog {
		p.mu.Lock()
		// 目录为空时按短退避强制刷新。后台周期用的 15 分钟间隔不能直接用在请求路径上:
		// refreshCatalog 见到 attemptedAt/catalogErr 就会跳过, 沿用间隔会让该 provider
		// 一直 400 到下一个后台周期。
		need := len(p.catalog) == 0 && time.Now().UnixMilli()-p.attemptedAt >= providerCatalogRetryMs
		p.mu.Unlock()
		if need {
			ctx, cancel := context.WithTimeout(r.Context(), providerCatalogTimeout)
			if err := p.refreshCatalog(ctx, true); err != nil {
				log.Printf("  providers: request-path catalog refresh (%s) failed: %v", name, err)
			}
			cancel()
		}
	}

	if !p.isFree(pm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{
				"message": fmt.Sprintf("model %q is not a free model on provider %q", model, name),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	isStream, _ := params["stream"].(bool)
	params["model"] = pm
	// 通用 Provider 也计入统计: 上游按 provider/<名> 归组, 模型记为 <名>:<模型>,
	// 这样「按上游」能看出是哪个 Provider 在消耗 token。
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     providerUpstream(name),
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})
	status := http.StatusOK
	defer func() { tracker.finish(status < 400, status) }()

	resp, err := p.Chat(r.Context(), params, isStream)
	if err != nil {
		status = providerErrorStatus(err)
		log.Printf("  provider api error (%s): %v", name, err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	usageFn := func(u map[string]any) { tracker.observeUsage(u) }
	if isStream {
		handleStreamResponseWithUsage(w, resp, usageFn)
		return
	}
	handleNonStreamResponseWithUsage(w, resp, usageFn)
}
