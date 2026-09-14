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
		"model":      "xmodel-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
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

func TestZenResponsesStaticRouteBodyShape(t *testing.T) {
	// 回归: 静态规则模型(未登记但命中 muse-*-free)直接走 /responses 时,
	// 请求体必须是完整转换后的形态 —— input 非空、max_output_tokens 就位、
	// reasoning.effort 落地。此前因重复调用转换函数(把已经是 Responses 形态
	// 的体再当 chat 体转一次), input 被转成空数组, 上游回 400
	// "`input` must be non-empty"。
	resetZenResponsesFlavorState(t, t.TempDir()+"/zen-responses-only.json")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("静态规则模型应直打 /responses, 实际 %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"r","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	withTestZenBaseURL(t, srv.URL)

	resp, _, err := callZenAPI(context.Background(), map[string]any{
		"model":      "muse-static-free",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 300,
	}, false)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	resp.Body.Close()

	input, _ := got["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input 必须非空且含 1 条消息, got %#v (完整体: %#v)", got["input"], got)
	}
	if got["max_output_tokens"] != float64(300) {
		t.Fatalf("max_output_tokens 应为 300, got %#v", got["max_output_tokens"])
	}
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "low" {
		t.Fatalf("未指定 effort 时应默认 low, got %#v", got["reasoning"])
	}
	if _, hasMessages := got["messages"]; hasMessages {
		t.Fatal("发出前不应残留 chat 形态的 messages 字段")
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
