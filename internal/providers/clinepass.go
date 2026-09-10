package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"cline-go-proxy/internal/protocol"
)

// proxyDoer is set by the host app to route upstream calls through the proxy pool.
// Falls back to kit.HTTPClient when nil (no proxy configured).
var proxyDoer func(*http.Request) (*http.Response, error)

// SetProxyDoer registers the proxy-aware HTTP client from the main app.
func SetProxyDoer(fn func(*http.Request) (*http.Response, error)) {
	proxyDoer = fn
}

func doRequest(req *http.Request) (*http.Response, error) {
	if proxyDoer != nil {
		return proxyDoer(req)
	}
	return kit.HTTPClient.Do(req)
}

// clinepassProvider manages a pool of ClinePass API keys and routes
// requests to the Cline API with Bearer auth.
//
// Borrowed from hayou2002/clinepass-proxy (Python): key rotation with
// 429 cooldown, automatic recovery, and reasoning-field pass-through.
// Keys persist in .clinepass-keys.json next to the other data files.
type clinepassProvider struct {
	mu   sync.Mutex
	keys []*cpKey
	path string
}

// ponytail: fixed 300s cooldown like the upstream Python project; make it
// configurable only if real-world usage demands different values.
const clinepassCooldown = 300 * time.Second

type cpKey struct {
	Key         string    `json:"key"`
	Status      string    `json:"status"` // active, cooldown
	CooldownExp time.Time `json:"cooldownExp,omitempty"`
	UsageCount  int64     `json:"usageCount"`
	LastUsed    time.Time `json:"lastUsed"`
}

func NewClinePassProvider() *clinepassProvider {
	p := &clinepassProvider{
		path: kit.ResolveDataPath(".clinepass-keys.json"),
	}
	p.load()
	go p.recoveryLoop()
	return p
}

func (p *clinepassProvider) Kind() string { return "clinepass" }

func (p *clinepassProvider) load() {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var arr []*cpKey
	if json.Unmarshal(data, &arr) == nil {
		p.keys = arr
	}
}

func (p *clinepassProvider) save() {
	data, _ := json.MarshalIndent(p.keys, "", "  ")
	if err := os.WriteFile(p.path, data, 0600); err != nil {
		fmt.Printf("clinepass save keys: %v\n", err)
	}
}

func (p *clinepassProvider) recoveryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		now := time.Now()
		for _, k := range p.keys {
			if k.Status == "cooldown" && !k.CooldownExp.IsZero() && now.After(k.CooldownExp) {
				k.Status = "active"
				k.CooldownExp = time.Time{}
			}
		}
		p.mu.Unlock()
	}
}

func (p *clinepassProvider) pickActive() *cpKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, k := range p.keys {
		if k.Status == "cooldown" && !k.CooldownExp.IsZero() && now.After(k.CooldownExp) {
			k.Status = "active"
			k.CooldownExp = time.Time{}
		}
		if k.Status == "active" {
			k.UsageCount++
			k.LastUsed = now
			return k
		}
	}
	return nil
}

func (p *clinepassProvider) markCooldown(k *cpKey) {
	p.mu.Lock()
	k.Status = "cooldown"
	k.CooldownExp = time.Now().Add(clinepassCooldown)
	p.mu.Unlock()
	p.save()
}

func (p *clinepassProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := p.RawChat(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("clinepass decode: %w", err)
	}
	// ClinePass wraps payloads in {data: {...}}; unwrap one level.
	if d, ok := out["data"].(map[string]any); ok {
		out = d
	}
	usage, _ := out["usage"].(map[string]any)
	return &ChatResponse{Body: out, Status: http.StatusOK, Usage: usage}, nil
}

// RawChat performs the upstream call and returns the raw HTTP response so
// the host app can run its protocol-specific stream converters
// (Anthropic events, Responses events) over OpenAI-format SSE.
// Caller closes resp.Body.
func (p *clinepassProvider) RawChat(ctx context.Context, req ChatRequest, stream bool) (*http.Response, error) {
	k := p.pickActive()
	if k == nil {
		return nil, fmt.Errorf("no active clinepass keys; add one via /admin/api/clinepass/keys")
	}
	body := toClinePassBody(req, stream)
	resp, err := callClinePassAPI(ctx, body, k.Key)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		p.markCooldown(k)
		return nil, fmt.Errorf("clinepass key %s rate limited, cooldown %s", maskKey(k.Key), clinepassCooldown)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clinepass API %d: %s", resp.StatusCode, kit.Truncate(kit.ReadBody(resp), 300))
	}
	return resp, nil
}

func (p *clinepassProvider) ChatStream(ctx context.Context, req ChatRequest, w io.Writer, onUsage func(map[string]any)) error {
	resp, err := p.RawChat(ctx, req, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sawFinish := false // 上游是否已发过 finish_reason
	events, errc := protocol.ScanSSE(resp.Body)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// 上游断流(未发 [DONE]): 合成 finish_reason + [DONE] 收尾
				if err := protocol.AppendStopChunkIfNoFinish(w, sawFinish, req.Model); err != nil {
					return err
				}
				_, err := io.WriteString(w, "data: [DONE]\n\n")
				return err
			}
			if ev.Done {
				if err := protocol.AppendStopChunkIfNoFinish(w, sawFinish, req.Model); err != nil {
					return err
				}
				_, err := io.WriteString(w, "data: [DONE]\n\n")
				return err
			}
			if ev.Payload != nil {
				if !sawFinish && protocol.HasFinishReason(ev.Payload) {
					sawFinish = true
				}
				if onUsage != nil {
					if u, ok := ev.Payload["usage"].(map[string]any); ok && len(u) > 0 {
						onUsage(u)
					}
				}
				// reasoning/reasoning_content fields pass through untouched.
				norm := protocol.NormalizeOpenAIChunk(ev.Payload)
				b, err := json.Marshal(norm)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
					return err
				}
			}
		case err := <-errc:
			return err
		}
	}
}

func (p *clinepassProvider) ListModels() []ModelInfo {
	models := []struct {
		id       string
		ctx, out int
	}{
		{"deepseek-v4-flash", 1024000, 384000},
		{"deepseek-v4-pro", 1024000, 384000},
		{"glm-5.2", 1000000, 128000},
		{"kimi-k2.7-code", 256000, 256000},
		{"kimi-k2.6", 256000, 256000},
		{"kimi-k3", 1024000, 128000},
		{"qwen3.7-max", 1000000, 128000},
		{"qwen3.7-plus", 1000000, 128000},
		{"minimax-m3", 1000000, 128000},
		{"mimo-v2.5", 1024000, 128000},
		{"mimo-v2.5-pro", 1024000, 128000},
	}
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, ModelInfo{
			ID:       "cline-pass/" + m.id,
			Context:  m.ctx,
			Output:   m.out,
			Source:   "clinepass",
			Cost:     "subscription",
			Provider: "clinepass",
		})
	}
	return out
}

func (p *clinepassProvider) HealthCheck(ctx context.Context) error {
	p.mu.Lock()
	n := len(p.keys)
	active := 0
	for _, k := range p.keys {
		if k.Status == "active" {
			active++
		}
	}
	p.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("no keys configured")
	}
	if active == 0 {
		return fmt.Errorf("all keys in cooldown")
	}
	return nil
}

// AddKey / RemoveKey / KeyStatuses back the admin REST API.
func (p *clinepassProvider) AddKey(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Key == key {
			k.Status = "active"
			k.CooldownExp = time.Time{}
			return
		}
	}
	p.keys = append(p.keys, &cpKey{Key: key, Status: "active"})
	p.save()
}

func (p *clinepassProvider) RemoveKey(masked string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, k := range p.keys {
		if maskKey(k.Key) == masked || k.Key == masked {
			p.keys = append(p.keys[:i], p.keys[i+1:]...)
			p.save()
			return
		}
	}
}

func (p *clinepassProvider) KeyStatuses() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, map[string]any{
			"key":      maskKey(k.Key),
			"status":   k.Status,
			"cooldown": k.CooldownExp,
			"usage":    k.UsageCount,
		})
	}
	return out
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

func toClinePassBody(req ChatRequest, stream bool) map[string]any {
	body := map[string]any{"model": req.Model}
	if len(req.Messages) > 0 {
		var msgs any
		if json.Unmarshal(req.Messages, &msgs) == nil {
			body["messages"] = msgs
		}
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	for _, k := range []string{"temperature", "top_p", "tools", "tool_choice", "stop"} {
		if v, ok := req.Extra[k]; ok {
			body[k] = v
		}
	}
	if stream {
		body["stream"] = true
	}
	return body
}

func callClinePassAPI(ctx context.Context, body map[string]any, key string) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return doRequest(req)
}
