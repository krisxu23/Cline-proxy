package app

// callZenAPI 的 Responses 自适应回退端到端验证:
// 假上游对 /chat/completions 一律 500(复刻 muse-free 家族的上游行为),
// 对 /responses 返回正常 Responses 响应 —— 首次请求应自动回退成功并登记,
// 后续请求应直接走 /responses。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func withTestZenBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := getZenConfig()
	cfg := *orig
	cfg.BaseURL = url
	cfg.BaseURLs = []string{url}
	cfg.Retries = 1
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(orig) })
}

func resetZenResponsesFlavorState(t *testing.T, file string) {
	t.Helper()
	setZenResponsesOnlyFileForTest(file)
	t.Cleanup(func() {
		setZenResponsesOnlyFileForTest("")
		zenRespOnlyMu.Lock()
		zenRespOnly = map[string]bool{}
		zenChatOnlyMemo = map[string]bool{}
		zenRespOnlyLoaded = false
		zenRespOnlyMu.Unlock()
	})
}

func TestZenResponsesFallbackEndToEnd(t *testing.T) {
	resetZenResponsesFlavorState(t, t.TempDir()+"/zen-responses-only.json")

	var chatHits, respHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			chatHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"type":"error","error":{"type":"error","message":"Internal server error"}}`))
		case "/responses":
			respHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"resp_x","created_at":1789362831,"status":"completed","model":"xmodel-free",
				"output":[{"type":"message","content":[{"type":"output_text","text":"hi from responses"}]}],
				"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withTestZenBaseURL(t, srv.URL)

	params := map[string]any{
		"model":     "xmodel-free",
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16,
	}
	resp, _, err := callZenAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("自适应回退应成功, got err: %v", err)
	}
	defer resp.Body.Close()

	// 响应必须已被翻译回 chat completions 形态
	var chat map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&chat); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if chat["object"] != "chat.completion" {
		t.Fatalf("应翻译为 chat.completion, got %v", chat["object"])
	}
	choices := chat["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hi from responses" {
		t.Fatalf("内容翻译错误: %v", msg["content"])
	}

	// 模型已被登记为 Responses 专用(含持久化)
	if !zenUseResponsesAPI("xmodel-free") {
		t.Fatal("回退成功后应登记该模型")
	}
	// 第二次请求应直接走 /responses, 不再浪费一次 chat 500
	if _, _, err := callZenAPI(context.Background(), params, false); err != nil {
		t.Fatalf("登记后的直接调用应成功: %v", err)
	}
	if chatHits.Load() != 1 {
		t.Fatalf("chat/completions 应只被击中 1 次(登记前), 实际 %d", chatHits.Load())
	}
	if respHits.Load() != 2 {
		t.Fatalf("/responses 应被击中 2 次, 实际 %d", respHits.Load())
	}
}

func TestZenResponsesFallbackNegativeMemo(t *testing.T) {
	resetZenResponsesFlavorState(t, t.TempDir()+"/zen-responses-only.json")

	var respHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"500"}`))
		case "/responses":
			respHits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"type":"error","error":{"type":"MissingSessionID","message":"nope"}}`))
		}
	}))
	defer srv.Close()
	withTestZenBaseURL(t, srv.URL)

	params := map[string]any{
		"model":    "chat-only-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	for i := 0; i < 2; i++ {
		resp, _, err := callZenAPI(context.Background(), params, false)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("两端点都坏时必须返回错误(第 %d 次)", i+1)
		}
	}
	if respHits.Load() != 1 {
		t.Fatalf("/responses 回退失败后应被进程内记忆, 只试 1 次, 实际 %d", respHits.Load())
	}
	if zenUseResponsesAPI("chat-only-model") {
		t.Fatal("回退失败不应登记为 Responses 专用")
	}
	if !zenChatOnlyKnown("chat-only-model") {
		t.Fatal("回退 4xx 后应记入进程内负向名单")
	}
}
