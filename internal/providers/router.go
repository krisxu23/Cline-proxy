package providers

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ClassifyFunc maps a model name to a provider kind. The host (internal/app)
// supplies it so providers stays free of app state; app already owns the
// zen/cline routing rules and failover state machine.
type ClassifyFunc func(model string) string

// Router dispatches requests to registered providers by kind. The kind is
// decided by a ClassifyFunc injected at construction time.
type Router struct {
	mu        sync.RWMutex
	registry  map[string]Provider
	classify  ClassifyFunc
	fallback  string // kind used when classify returns "" or unknown
}

func NewRouter(classify ClassifyFunc, fallback string) *Router {
	return &Router{
		registry: map[string]Provider{},
		classify: classify,
		fallback: fallback,
	}
}

// Register wires a provider under a kind; later registrations replace
// earlier ones (used by tests and admin toggles).
func (r *Router) Register(kind string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry[kind] = p
}

// KindOf resolves the provider kind for a model.
func (r *Router) KindOf(model string) string {
	kind := strings.TrimSpace(r.classify(model))
	if kind == "" {
		return r.fallback
	}
	return kind
}

// Provider returns the provider chosen for a model, or nil.
func (r *Router) Provider(model string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.registry[r.KindOf(model)]
}

// Chat delegates to the provider selected by req.Model.
func (r *Router) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	p := r.Provider(req.Model)
	if p == nil {
		return nil, fmt.Errorf("no provider registered for model %q", req.Model)
	}
	return p.Chat(ctx, req)
}

// ChatStream delegates to the provider selected by req.Model.
func (r *Router) ChatStream(ctx context.Context, req ChatRequest, w io.Writer, onUsage func(map[string]any)) error {
	p := r.Provider(req.Model)
	if p == nil {
		return fmt.Errorf("no provider registered for model %q", req.Model)
	}
	return p.ChatStream(ctx, req, w, onUsage)
}

// ListAllModels aggregates models across every registered provider,
// skipping providers that report none.
func (r *Router) ListAllModels() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []ModelInfo{}
	for _, p := range r.registry {
		out = append(out, p.ListModels()...)
	}
	return out
}
