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

const providerAttemptTimeout = 180 * time.Second

// providerDirectClient 直连上游(默认用于非 Gemini provider)。
var providerDirectClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// providerProxiedClient 走系统代理(用于需要海外出口的 Gemini)。
var providerProxiedClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	},
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
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal provider body: %w", err)
	}
	u := strings.TrimRight(cfg.BaseURL, "/") + cfg.chatPath()
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	origin := localOrigin()
	for k, spec := range cfg.Headers {
		if v := cfg.resolveHeader(spec, origin); v != "" {
			req.Header.Set(k, v)
		}
	}

	client := providerDirectClient
	if providerUsesProxiedClient(p) {
		client = providerProxiedClient
	}
	if !stream {
		ctx, cancel := context.WithTimeout(ctx, providerAttemptTimeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		p.recordRejection(model, resp.StatusCode, body)
		return nil, &providerError{Provider: p.name, Status: resp.StatusCode, Body: string(body)}
	}
	if needsSig {
		if stream {
			ext := newSignatureStreamExtractor(p.sigCache)
			resp.Body = &sseTapReader{rc: resp.Body, onLine: func(line string) { ext.push(line) }}
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				rememberSignaturesFromPayload(parsed, p.sigCache)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
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
	if !p.isFree(pm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{
				"message": fmt.Sprintf("model %q is not a free model on provider %q", model, name),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	// 目录型 provider 首次请求前同步刷新一次, 之后由后台循环维护
	if cfg.Catalog {
		p.mu.Lock()
		need := len(p.catalog) == 0 && p.catalogErr == ""
		p.mu.Unlock()
		if need {
			ctx, cancel := context.WithTimeout(r.Context(), providerCatalogTimeout)
			if err := p.refreshCatalog(ctx, false); err != nil {
				log.Printf("  providers: initial catalog refresh (%s) failed: %v", name, err)
			}
			cancel()
		}
	}

	isStream, _ := params["stream"].(bool)
	params["model"] = pm
	resp, err := p.Chat(r.Context(), params, isStream)
	if err != nil {
		status := providerErrorStatus(err)
		log.Printf("  provider api error (%s): %v", name, err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()
	usageFn := func(map[string]any) {}
	if isStream {
		handleStreamResponseWithUsage(w, resp, usageFn)
		return
	}
	handleNonStreamResponseWithUsage(w, resp, usageFn)
}
