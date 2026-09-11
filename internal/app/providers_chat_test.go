package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseProviderModel(t *testing.T) {
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://x", APIKey: "k"})
	if name, rest, ok := parseProviderModel("openrouter:z-ai/glm-5.3:free"); !ok || name != "openrouter" || rest != "z-ai/glm-5.3:free" {
		t.Fatalf("parse: %q %q %v", name, rest, ok)
	}
	if _, _, ok := parseProviderModel("mimo-v2.5-free"); ok {
		t.Fatal("bare model name must not parse as a provider route")
	}
	if _, _, ok := parseProviderModel("nope:model"); ok {
		t.Fatal("unknown provider must not parse")
	}
	if _, _, ok := parseProviderModel("openrouter:"); ok {
		t.Fatal("empty model must not parse")
	}
	if _, _, ok := parseProviderModel("z-ai/glm-5.3:free"); ok {
		t.Fatal("model id containing a colon must not parse as a provider route")
	}
}

func TestProviderErrorStatus(t *testing.T) {
	if got := providerErrorStatus(&providerError{Status: 403}); got != 403 {
		t.Fatalf("4xx must pass through: %d", got)
	}
	if got := providerErrorStatus(&providerError{Status: 503}); got != 502 {
		t.Fatalf("5xx must map to 502: %d", got)
	}
	if got := providerErrorStatus(context.DeadlineExceeded); got != 502 {
		t.Fatalf("network error must map to 502: %d", got)
	}
}

func TestProviderChatPassthrough(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModel, _ = req["model"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}}},
		})
	}))
	defer srv.Close()

	setTestProvider(t, "bai", providerConfig{BaseURL: srv.URL, APIKey: "k-bai", FreeModels: []string{"glm-5.3-flash"}})
	p := providerByName("bai")
	resp, err := p.Chat(context.Background(), map[string]any{
		"model":    "bai:glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer k-bai" {
		t.Fatalf("authorization: %q", gotAuth)
	}
	if gotModel != "glm-5.3-flash" {
		t.Fatalf("provider prefix must be stripped: %q", gotModel)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestProviderChatCustomHeaders(t *testing.T) {
	var gotReferer, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "or", providerConfig{
		BaseURL: srv.URL, APIKey: "k",
		Headers: map[string]providerHeaderSpec{
			"HTTP-Referer": {Default: "${origin}"},
			"X-Title":      {Default: "Cline Proxy"},
		},
	})
	p := providerByName("or")
	resp, err := p.Chat(context.Background(), map[string]any{"model": "m", "messages": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotTitle != "Cline Proxy" {
		t.Fatalf("static header: %q", gotTitle)
	}
	if gotReferer == "" {
		t.Fatal("${origin} must expand")
	}
}

func TestProviderChatErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()
	setTestProvider(t, "x", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"m"}})
	p := providerByName("x")
	_, err := p.Chat(context.Background(), map[string]any{"model": "m", "messages": []any{}}, false)
	if err == nil {
		t.Fatal("403 must return an error")
	}
	pe, ok := err.(*providerError)
	if !ok || pe.Status != 403 {
		t.Fatalf("error must carry upstream status: %#v", err)
	}
	if got := providerErrorStatus(err); got != 403 {
		t.Fatalf("status passthrough: %d", got)
	}
}

func TestProviderChatRecordsPermanentRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"model is no longer available"}}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gone", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"dead"}})
	p := providerByName("gone")
	if _, err := p.Chat(context.Background(), map[string]any{"model": "dead", "messages": []any{}}, false); err == nil {
		t.Fatal("404 must return an error")
	}
	if p.isFree("dead") {
		t.Fatal("permanently rejected model must drop out of the free set")
	}
}

func TestProviderChatInjectsThoughtSignature(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gemini", providerConfig{BaseURL: srv.URL, APIKey: "gk", FreeModels: []string{"g1"}})
	p := providerByName("gemini")
	params := map[string]any{
		"model": "g1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
		},
	}
	resp, err := p.Chat(context.Background(), params, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %+v", msgs)
	}
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls[0].(map[string]any)); got != skipThoughtSignature {
		t.Fatalf("sentinel must be injected: %q", got)
	}
}

func TestProviderChatRemembersSignatureFromResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call-9","extra_content":{"google":{"thought_signature":"sig-9"}}}]}}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gemini", providerConfig{BaseURL: srv.URL, APIKey: "gk", FreeModels: []string{"g1"}})
	p := providerByName("gemini")
	resp, err := p.Chat(context.Background(), map[string]any{"model": "g1", "messages": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := p.sigCache.lookup("call-9"); got != "sig-9" {
		t.Fatalf("response signature must be remembered: %q", got)
	}
}

func TestSSETapReaderSplitsLines(t *testing.T) {
	// 中间分片故意不带换行: 跨 chunk 的行重组必须由 pending 缓冲完成;
	// 末片携带空行事件分隔符, 断言第三行为空字符串。
	body := io.NopCloser(&chunkReader{chunks: []string{"data: a\n", "data: b", "\n\n"}})
	var lines []string
	tap := &sseTapReader{rc: body, onLine: func(l string) { lines = append(lines, l) }}
	io.ReadAll(tap)
	tap.Close()
	want := []string{"data: a", "data: b", ""}
	if len(lines) != len(want) {
		t.Fatalf("lines: %#v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: %q want %q", i, lines[i], want[i])
		}
	}
}

func TestProviderChatNonStreamBodyFullyRead(t *testing.T) {
	// 非流式响应必须在 Chat 内部读完: 请求上下文控制整个响应生命周期,
	// 返回后 defer cancel 会截断未读完的 body(超过传输层缓冲的部分丢失)。
	big := strings.Repeat("x", 200<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": big}}},
		})
	}))
	defer srv.Close()
	setTestProvider(t, "big", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"m1"}})
	p := providerByName("big")
	resp, err := p.Chat(context.Background(), map[string]any{"model": "m1", "messages": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body read must not fail after Chat returns: %v", err)
	}
	if !strings.Contains(string(body), big) {
		t.Fatalf("body was truncated: got %d bytes", len(body))
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body must be complete JSON: %v", err)
	}
}

func TestHandleProviderChatRejectsPaidModel(t *testing.T) {
	setTestProvider(t, "bai", providerConfig{BaseURL: "https://x", APIKey: "k", FreeModels: []string{"free-one"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", nil)
	handleProviderChat(rec, req, map[string]any{"model": "bai:paid-one", "messages": []any{}}, "bai")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("paid model must be rejected: %d", rec.Code)
	}
}

func TestHandleProviderChatRejectsKeylessProvider(t *testing.T) {
	setTestProvider(t, "nokey", providerConfig{BaseURL: "https://x", FreeModels: []string{"m"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", nil)
	handleProviderChat(rec, req, map[string]any{"model": "nokey:m", "messages": []any{}}, "nokey")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("keyless provider must be 503: %d", rec.Code)
	}
}

func TestHandleProviderChatRefreshesCatalogBeforeGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "cat-free", "pricing": map[string]any{"prompt": "0", "completion": "0"}},
			}})
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "cat", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("cat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", nil)
	handleProviderChat(rec, req, map[string]any{"model": "cat:cat-free", "messages": []any{}}, "cat")
	if rec.Code != http.StatusOK {
		t.Fatalf("first request must refresh the catalog and pass the gate: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(p.catalog) == 0 {
		t.Fatal("catalog should be populated by the first request")
	}
}

func TestProviderChatReplaysOnRejectedSignature(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var secondBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests++
		n := requests
		if n >= 2 {
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				secondBody = parsed
			}
		}
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"missing thought_signature for tool call"}}`))
			return
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	setTestProvider(t, "gemini", providerConfig{BaseURL: srv.URL, APIKey: "gk", FreeModels: []string{"g1"}})
	p := providerByName("gemini")
	params := map[string]any{
		"model": "g1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
		},
	}
	resp, err := p.Chat(context.Background(), params, false)
	if err != nil {
		t.Fatalf("rejected signature must be replayed, got error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("stub must see exactly two requests, got %d", requests)
	}
	if secondBody == nil {
		t.Fatal("second request body was not captured")
	}
	msgs, _ := secondBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("second request messages: %+v", msgs)
	}
	calls, _ := msgs[1].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("second request tool calls: %+v", calls)
	}
	if got := readThoughtSignature(calls[0].(map[string]any)); got != skipThoughtSignature {
		t.Fatalf("replay must carry the skip sentinel: %q", got)
	}
}

// chunkReader 按固定分片返回数据, 模拟网络分片边界。
type chunkReader struct {
	chunks []string
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}
