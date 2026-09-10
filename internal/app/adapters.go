package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"cline-go-proxy/internal/providers"
)

// gateway assembles the provider layer: the Router seam plus concrete
// providers. cline/zen keep their battle-tested call paths in this package
// and are exposed through thin adapters; clinepass is fully owned by the
// providers package (key pool + cooldown + RawChat for protocol reuse).
type gateway struct {
	Router      *providers.Router
	ClinePass   *providers.ClinePassProvider
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
	resp, acc, err := callClineAPIFailover(params, false)
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
	// ponytail: the existing relay writes headers+flushes, so only
	// http.ResponseWriter is supported; Router callers pass one in practice.
	rw, ok := w.(http.ResponseWriter)
	if !ok {
		return fmt.Errorf("cline streaming requires http.ResponseWriter")
	}
	params := chatRequestToParams(req)
	resp, _, err := callClineAPIFailover(params, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	handleStreamResponseWithUsage(rw, resp, onUsage)
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
	// ponytail: same ResponseWriter constraint as clineAdapter.
	rw, ok := w.(http.ResponseWriter)
	if !ok {
		return fmt.Errorf("zen streaming requires http.ResponseWriter")
	}
	params := chatRequestToParams(req)
	resp, _, err := callZenAPI(ctx, params, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	handleStreamResponseWithUsage(rw, resp, onUsage)
	return nil
}

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
func chatRequestToParams(req providers.ChatRequest) map[string]any {
	params := map[string]any{}
	if req.Model != "" {
		params["model"] = req.Model
	}
	params["stream"] = req.Stream
	if req.MaxTokens > 0 {
		params["max_tokens"] = float64(req.MaxTokens)
	}
	if req.Temperature != 0 {
		params["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		params["top_p"] = req.TopP
	}
	if len(req.Messages) > 0 {
		var msgs any
		if json.Unmarshal(req.Messages, &msgs) == nil {
			params["messages"] = msgs
		}
	}
	for k, v := range req.Extra {
		params[k] = v
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
