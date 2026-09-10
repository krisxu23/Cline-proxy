package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeProvider struct {
	kind    string
	chatErr error
	streamN int // number of bytes written to stream writer
}

func (f *fakeProvider) Kind() string { return f.kind }
func (f *fakeProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	return &ChatResponse{Body: map[string]any{"model": req.Model}, Status: 200}, nil
}
func (f *fakeProvider) ChatStream(ctx context.Context, req ChatRequest, w io.Writer, onUsage func(map[string]any)) error {
	n, _ := w.Write([]byte("data: test\n\n"))
	f.streamN = n
	return nil
}
func (f *fakeProvider) ListModels() []ModelInfo { return nil }
func (f *fakeProvider) HealthCheck(ctx context.Context) error {
	return f.chatErr
}

func TestRouterDispatch(t *testing.T) {
	r := NewRouter(func(model string) string {
		switch {
		case strings.HasPrefix(model, "cline-pass/"):
			return "clinepass"
		case strings.HasSuffix(model, "-free"):
			return "zen"
		default:
			return "cline"
		}
	}, "cline")

	cline := &fakeProvider{kind: "cline"}
	zen := &fakeProvider{kind: "zen"}
	pass := &fakeProvider{kind: "clinepass"}
	r.Register("cline", cline)
	r.Register("zen", zen)
	r.Register("clinepass", pass)

	// Chat routing
	if _, err := r.Chat(context.Background(), ChatRequest{Model: "deepseek/deepseek-v4-flash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-flash-free"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Chat(context.Background(), ChatRequest{Model: "cline-pass/glm-5.2"}); err != nil {
		t.Fatal(err)
	}

	// Unknown model falls back to default kind
	if _, err := r.Chat(context.Background(), ChatRequest{Model: "whatever"}); err != nil {
		t.Fatal(err)
	}

	// Unregistered kind surfaces a clear error
	r2 := NewRouter(func(string) string { return "missing" }, "missing")
	if _, err := r2.Chat(context.Background(), ChatRequest{Model: "x"}); err == nil || !strings.Contains(err.Error(), "no provider registered") {
		t.Fatalf("want no-provider error, got %v", err)
	}

	// Stream routing writes through
	var buf bytes.Buffer
	if err := r.ChatStream(context.Background(), ChatRequest{Model: "x-free"}, &buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "data: test") {
		t.Fatalf("stream output: %q", buf.String())
	}
}

func TestRouterNilProvider(t *testing.T) {
	r := NewRouter(func(string) string { return "cline" }, "cline")
	// nothing registered
	err := r.ChatStream(context.Background(), ChatRequest{Model: "m"}, &bytes.Buffer{}, nil)
	if err == nil {
		t.Fatal("expected error for unregistered provider")
	}
	if !errors.Is(err, err) {
		t.Fatal("unreachable")
	}
}

func TestClinePassKeyPool(t *testing.T) {
	p := NewClinePassProvider()
	p.path = t.TempDir() + "/keys.json" // avoid touching real data file
	p.keys = nil                        // ignore any keys loaded from disk

	if err := p.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error with empty pool")
	}

	p.AddKey("sk-test-12345678")
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Fatalf("health after add: %v", err)
	}
	statuses := p.KeyStatuses()
	if len(statuses) != 1 || statuses[0]["status"] != "active" {
		t.Fatalf("statuses: %#v", statuses)
	}

	// Duplicate add reactivates instead of duplicating
	p.AddKey("sk-test-12345678")
	if len(p.KeyStatuses()) != 1 {
		t.Fatal("duplicate key added")
	}

	// Cooldown a key then verify recovery
	k := p.keys[0]
	p.markCooldown(k)
	if p.pickActive() != nil {
		t.Fatal("cooldown key should not be picked")
	}
	// RemoveKey by mask
	p.RemoveKey(maskKey("sk-test-12345678"))
	if len(p.KeyStatuses()) != 0 {
		t.Fatal("remove failed")
	}
}
