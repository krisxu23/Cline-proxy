package app

// P2-4 + P2-5 回归锁定。
//
// P2-4: 凭据必须按**本次真实出口**选(zen_call.go 在 pickUnifiedExit/setReqExit
// 之后按 exit 重选 key)。首试时 reqExitKey(ctx) 为空 → 预选只能拿到 keys[0];
// 一旦出口已知, 同一出口必须永远用同一把 key, 否则 (key, 出口) 配对抖动。
// P2-5: 文本匹配(not found/unsupported)仅限 404 生效; 负向 memo 有 30 分钟
// TTL 且配置重载时清空。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// P2-4: 同一出口反复选必须同一 key(含首试为空的退化情形)。
func TestZenP24_SameExitSameKey(t *testing.T) {
	zenClearRetiredKeysForTest()
	defer zenClearRetiredKeysForTest()

	cfg := &zenConfigData{Key: "k1", Keys: []string{"k2", "k3"}}
	exits := []string{"", "socks5://1.2.3.4:1080", "vmess://node-x", "http://5.6.7.8:8080"}
	for _, exit := range exits {
		first := zenSelectKeyForModel(cfg, exit, "some-paid-model")
		for i := 0; i < 20; i++ {
			if got := zenSelectKeyForModel(cfg, exit, "some-paid-model"); got != first {
				t.Fatalf("P2-4: 出口 %q 的 key 不稳定: %q vs %q", exit, first, got)
			}
		}
	}
	// 首试(reqExit 为空)的预选必须是 keys[0] —— 调用方发请求头时不能裸奔,
	// 真实出口已知后由调用方按 exit 重选(zen_call.go reqKeyRefresh)。
	if got := zenSelectKeyForModel(cfg, "", "some-paid-model"); got != "k1" {
		t.Fatalf("P2-4: 首试空出口应预选第一把 key k1, 实得 %q", got)
	}
}

// P2-4 端到端: 直连(真实出口为空)发出的 Authorization 必须与
// zenSelectKeyForModel(cfg, "", model) 一致 —— 锁住"首试为空"路径的请求头。
func TestZenP24_RequestHeaderMatchesEmptyExitKey(t *testing.T) {
	zenClearRetiredKeysForTest()
	defer zenClearRetiredKeysForTest()
	resetZenResponsesFlavorState(t, t.TempDir()+"/zen-responses-only.json")

	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	withTestZenBaseURL(t, srv.URL)

	orig := getZenConfig()
	cfg := *orig
	cfg.Key = "k1"
	cfg.Keys = []string{"k2", "k3"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(orig) })

	params := map[string]any{
		"model":      "p24-paid-model",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16,
	}
	resp, _, err := callZenAPI(context.Background(), params, false)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	resp.Body.Close()

	want := "Bearer " + zenSelectKeyForModel(&cfg, "", "p24-paid-model")
	if got, _ := gotAuth.Load().(string); got != want {
		t.Fatalf("P2-4: 请求头 %q 应与空出口所选 %q 一致", got, want)
	}
}

// P2-5: 文本匹配仅限 404 生效 —— 非 404 即使正文含 not found/unsupported 也不算。
func TestZenP25_EndpointUnsupportedOnlyOn404(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{404, `{"error":"model not found on this endpoint"}`, true},
		{404, `{"error":"unsupported endpoint for model"}`, true},
		{404, ``, true}, // 404 本身即证据, 与正文无关
		{401, `{"error":"Invalid API key: model not found?"}`, false},
		{429, `{"error":"quota exceeded, model not found in pool"}`, false},
		{500, `{"error":"Internal server error: unsupported shape"}`, false},
		{400, `{"error":"unsupported value"}`, false},
		{200, `{"error":"not found"}`, false},
	}
	for _, c := range cases {
		if got := zenEndpointUnsupported(c.status, c.body); got != c.want {
			t.Fatalf("P2-5: zenEndpointUnsupported(%d, %q) = %v, 期望 %v",
				c.status, c.body, got, c.want)
		}
	}
}

// P2-5: 非 404 的回退失败即使正文含关键词也不得记负向 memo。
func TestZenP25_Non404FallbackFailureNoMemo(t *testing.T) {
	resetZenResponsesFlavorState(t, t.TempDir()+"/zen-responses-only.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 429 正文故意含 "not found" —— 旧语义会误记 memo, 新语义不得记。
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited, model not found in quota pool"}`))
	}))
	defer srv.Close()

	cli := srv.Client()
	resp := tryZenResponsesFallback(context.Background(), srv.URL,
		map[string]any{"model": "p25-model", "input": "hi"}, false, cli, "k1", 16)
	if resp != nil {
		resp.Body.Close()
		t.Fatal("429 回退应返回 nil")
	}
	if zenChatOnlyKnown("p25-model") {
		t.Fatal("P2-5: 非 404 失败不得记入负向 memo")
	}
}

// P2-5: 负向 memo 30 分钟 TTL —— 到期自动失效, 回退恢复尝试。
func TestZenP25_ChatOnlyMemoTTL(t *testing.T) {
	zenClearChatOnlyMemoForTest()
	defer zenClearChatOnlyMemoForTest()

	zenMemoChatOnly("p25-ttl-model")
	if !zenChatOnlyKnown("p25-ttl-model") {
		t.Fatal("刚记入的 memo 应命中")
	}
	// 人工老化超过 TTL, 应视为过期。
	zenRespOnlyMu.Lock()
	zenChatOnlyMemo["p25-ttl-model"] = time.Now().Add(-zenChatOnlyMemoTTL - time.Minute)
	zenRespOnlyMu.Unlock()
	if zenChatOnlyKnown("p25-ttl-model") {
		t.Fatalf("P2-5: 超过 TTL(%v)的 memo 应过期", zenChatOnlyMemoTTL)
	}
}

// P2-5: 配置重载(setZenConfig 经 applyZenConfigSideEffects)清空负向 memo。
func TestZenP25_ConfigReloadClearsMemo(t *testing.T) {
	zenClearChatOnlyMemoForTest()
	defer zenClearChatOnlyMemoForTest()

	zenMemoChatOnly("p25-reload-model")
	if !zenChatOnlyKnown("p25-reload-model") {
		t.Fatal("刚记入的 memo 应命中")
	}
	orig := getZenConfig()
	setZenConfig(orig) // 触发 applyZenConfigSideEffects → clearZenChatOnlyMemo
	t.Cleanup(func() { setZenConfig(orig) })
	if zenChatOnlyKnown("p25-reload-model") {
		t.Fatal("P2-5: 配置重载后负向 memo 应被清空")
	}
}
