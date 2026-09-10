package providers

import (
	"context"
	"encoding/json"
	"io"
)

// ChatRequest is the normalized, protocol-agnostic request shape that all
// providers accept. It is produced by internal/app/proxy.go after
// converting OpenAI / Anthropic / Responses to this common form.
type ChatRequest struct {
	Model       string         `json:"model"`
	Messages    json.RawMessage `json:"messages"`
	Stream      bool           `json:"stream"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature float64        `json:"temperature,omitempty"`
	TopP        float64        `json:"top_p,omitempty"`
	TopK        int            `json:"top_k,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	// Extra holds any fields not listed above (e.g. reasoning_effort,
	// response_format, etc.) so providers can forward them unchanged.
	Extra map[string]any `json:"-"`
}

// ChatResponse is what providers return for non-streaming calls. The
// caller is responsible for converting to the client protocol
// (OpenAI/Anthropic/Responses) via internal/protocol.
type ChatResponse struct {
	Body   map[string]any
	Status int
	Usage  map[string]any // optional; providers may fill for stats
}

// Provider is the minimal interface every upstream must implement.
type Provider interface {
	// Kind returns a short identifier used in logs and admin UI.
	Kind() string

	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ChatStream performs a streaming completion. The provider writes
	// SSE events directly to w; the caller does not need to parse them.
	// onUsage is called when a usage chunk arrives (may be nil).
	ChatStream(ctx context.Context, req ChatRequest, w io.Writer, onUsage func(map[string]any)) error

	// ListModels returns available models for this provider.
	ListModels() []ModelInfo

	// HealthCheck returns nil if the provider is reachable.
	HealthCheck(ctx context.Context) error
}

// ModelInfo is a minimal model descriptor for /v1/models aggregation.
type ModelInfo struct {
	ID       string `json:"id"`
	Context  int    `json:"context,omitempty"`
	Output   int    `json:"output,omitempty"`
	Source   string `json:"source,omitempty"`   // "cline", "zen", "clinepass"
	Cost     string `json:"cost,omitempty"`     // "free", "paid", "subscription"
	Provider string `json:"provider,omitempty"` // "cline", "zen", "clinepass"
}

// ClinePassProvider is the concrete ClinePass key-pool provider; the host
// app uses it for key management endpoints beyond the Provider interface.
type ClinePassProvider = clinepassProvider