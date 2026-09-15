package app

// 请求轨迹 (reqTrace) 与决策轨迹的测试:
// 覆盖 P0-1/2/3 的核心契约 —— 请求 id 关联、TTFT、上游与模型、跳过轨迹、
// 多协议 usage 归一、NULL vs 0 区分、决策 ring 的 TTL/LRU。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewRequestIDPrefersSafeClientValue(t *testing.T) {
	if got := newRequestID("trace-abc-123"); got != "trace-abc-123" {
		t.Fatalf("应沿用客户端请求 id, got %q", got)
	}
	// 含控制字符/非 ASCII 的客户端值不可信, 必须换成自生成 id
	for _, bad := range []string{"bad id with space", "含中文", "tab\there", strings.Repeat("x", 100)} {
		got := newRequestID(bad)
		if got == bad {
			t.Fatalf("不安全/超长的客户端 id 不应被沿用: %q", bad)
		}
		if !strings.HasPrefix(got, "req_") {
			t.Fatalf("应生成 req_ 前缀 id, got %q", got)
		}
	}
	if a, b := newRequestID(""), newRequestID(""); a == b {
		t.Fatal("两次自生成 id 不应相同")
	}
}

func TestReqTraceSnapshotAndSkips(t *testing.T) {
	tr := &reqTrace{RequestID: "req_x"}
	tr.SetUpstream("zen", "muse-spark-1.3-contributor-free")
	tr.AddAttempt()
	tr.AddAttempt()
	tr.AddSkip("zen/a", "冷却中(rateLimit)")
	tr.AddSkip("cline/*", "永久剔除")
	tr.SetProtocol("openai", true)
	tr.SetTokens(10, 20, 5, 3)
	tr.SetError("server_error", strings.Repeat("x", 400))

	s := tr.snapshot()
	if s.Upstream != "zen" || s.Resolved != "muse-spark-1.3-contributor-free" {
		t.Fatalf("上游/模型回填错误: %+v", s)
	}
	if s.Attempts != 2 {
		t.Fatalf("尝试次数应为 2, got %d", s.Attempts)
	}
	if len(s.Skipped) != 2 || s.Skipped[0] != "zen/a=冷却中(rateLimit)" {
		t.Fatalf("跳过轨迹格式不符: %v", s.Skipped)
	}
	if !s.Stream || s.Protocol != "openai" {
		t.Fatalf("协议/流式标记错误: %+v", s)
	}
	if s.PromptTokens != 10 || s.CompletionTokens != 20 || s.ReasoningTokens != 5 || s.CacheTokens != 3 {
		t.Fatalf("token 回填错误: %+v", s)
	}
	if !s.UsageReported {
		t.Fatal("上报过 usage 应置位")
	}
	if len([]rune(s.ErrMsg)) > 301 {
		t.Fatalf("错误消息应被截断, 长度 %d", len([]rune(s.ErrMsg)))
	}
	// nil 轨迹上所有回填必须安全
	var nilTrace *reqTrace
	nilTrace.SetUpstream("x", "y")
	nilTrace.AddAttempt()
	nilTrace.AddSkip("a", "b")
	nilTrace.SetError("c", "d")
	nilTrace.SetTokens(1, 2, 3, 4)
	if s2 := nilTrace.snapshot(); s2.RequestID != "" {
		t.Fatal("nil 轨迹快照应为零值")
	}
}

func TestParseUsageFieldsAcrossProtocols(t *testing.T) {
	cases := []struct {
		name           string
		usage          map[string]any
		p, c, r, cache int
		ok             bool
	}{
		{
			name: "OpenAI chat",
			usage: map[string]any{
				"prompt_tokens": float64(100), "completion_tokens": float64(50),
				"prompt_tokens_details":     map[string]any{"cached_tokens": float64(40)},
				"completion_tokens_details": map[string]any{"reasoning_tokens": float64(25)},
			},
			p: 100, c: 50, r: 25, cache: 40, ok: true,
		},
		{
			name: "OpenAI Responses",
			usage: map[string]any{
				"input_tokens": float64(200), "output_tokens": float64(80),
				"input_tokens_details":  map[string]any{"cached_tokens": float64(150)},
				"output_tokens_details": map[string]any{"reasoning_tokens": float64(60)},
			},
			p: 200, c: 80, r: 60, cache: 150, ok: true,
		},
		{
			name: "Anthropic cache 读写相加",
			usage: map[string]any{
				"input_tokens": float64(30), "output_tokens": float64(12),
				"cache_read_input_tokens": float64(1000), "cache_creation_input_tokens": float64(200),
			},
			p: 30, c: 12, cache: 1200, ok: true,
		},
		{
			name:  "报了零也算上报(区分 NULL 与 0)",
			usage: map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)},
			ok:    true,
		},
		{
			name:  "空对象不算上报",
			usage: map[string]any{},
			ok:    false,
		},
	}
	for _, tc := range cases {
		p, c, r, cache, ok := parseUsageFields(tc.usage)
		if ok != tc.ok || p != tc.p || c != tc.c || r != tc.r || cache != tc.cache {
			t.Fatalf("%s: got (p=%d c=%d r=%d cache=%d ok=%v), want (p=%d c=%d r=%d cache=%d ok=%v)",
				tc.name, p, c, r, cache, ok, tc.p, tc.c, tc.r, tc.cache, tc.ok)
		}
	}
}

func TestDecisionTraceRing(t *testing.T) {
	decisionTraceReset()
	defer decisionTraceReset()

	d := decisionTraceStart("req_1", "muse-spark-1.3-contributor-free")
	d.addCandidate("zen/a", "tried", "", 500, "server_error")
	d.addCandidate("zen/b", "skipped", "冷却中(rateLimit)", 0, "")
	d.addCandidate("cline/*", "tried", "", 200, "")
	d.setWinner("cline/*")
	d.finish()

	got := decisionTraceFor("req_1")
	if got == nil {
		t.Fatal("应能按请求 id 取回决策轨迹")
	}
	if got.Winner != "cline/*" || !got.Finished || len(got.Candidates) != 3 {
		t.Fatalf("轨迹内容不符: %+v", got)
	}
	if got.Candidates[1].Decision != "skipped" || got.Candidates[1].Reason == "" {
		t.Fatalf("跳过原因应保留: %+v", got.Candidates[1])
	}
	// 返回的是拷贝: 修改不应影响内部状态
	got.Candidates[0].Decision = "mutated"
	if again := decisionTraceFor("req_1"); again.Candidates[0].Decision != "tried" {
		t.Fatal("decisionTraceFor 必须返回拷贝")
	}
	if decisionTraceFor("") != nil || decisionTraceFor("nothing") != nil {
		t.Fatal("空 id / 不存在的 id 应返回 nil")
	}

	// LRU 上限: 超过 decisionTraceMaxKeep 条后最早的被淘汰
	for i := 0; i < decisionTraceMaxKeep+10; i++ {
		decisionTraceStart(newRequestID(""), "m")
	}
	if n := decisionTraceCount(); n > decisionTraceMaxKeep {
		t.Fatalf("ring 应受 LRU 上限约束, got %d", n)
	}
	if decisionTraceFor("req_1") != nil {
		t.Fatal("被淘汰的轨迹不应再取回")
	}
}

func TestRequestLogMiddlewareFillsTraceFields(t *testing.T) {
	closeReqLogs()
	reqLogsMu.Lock()
	reqLogs = nil
	reqLogsMu.Unlock()
	closeReqLogs()
	defer closeReqLogs()

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := traceFrom(r.Context())
		if tr == nil {
			t.Fatal("中间件必须注入请求轨迹")
		}
		if tr.snapshot().RequestID == "" {
			t.Fatal("轨迹里必须有请求 id")
		}
		tr.SetUpstream("zen", "muse-spark-1.3-contributor-free")
		tr.AddAttempt()
		tr.AddSkip("cline/*", "冷却中")
		tr.ObserveUsage(map[string]any{
			"prompt_tokens": float64(11), "completion_tokens": float64(22),
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(7)},
		})
		// 分两次写, 模拟流式: TTFT 取首次写出的时间
		w.WriteHeader(http.StatusOK)
		time.Sleep(12 * time.Millisecond)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/muse-spark-1.3-contributor-free"}`))
	req.Header.Set("X-Request-Id", "trace-keep-me")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "trace-keep-me" {
		t.Fatalf("响应头应回写请求 id, got %q", got)
	}
	flushReqLogs()
	logs := LoadRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("应记录 1 条请求日志, got %d", len(logs))
	}
	l := logs[0]
	if l.ID != "trace-keep-me" {
		t.Fatalf("日志应带请求 id, got %q", l.ID)
	}
	if l.Upstream != "zen" || l.ResolvedModel != "muse-spark-1.3-contributor-free" {
		t.Fatalf("上游/实际模型未落盘: %+v", l)
	}
	if l.Attempts != 1 || len(l.Skipped) != 1 {
		t.Fatalf("尝试/跳过轨迹未落盘: attempts=%d skipped=%v", l.Attempts, l.Skipped)
	}
	if l.PromptTokens != 11 || l.CompletionTokens != 22 || l.ReasoningTokens != 7 || !l.UsageReported {
		t.Fatalf("token 未落盘或未标上报: %+v", l)
	}
	if l.Protocol != "openai" {
		t.Fatalf("协议应判定为 openai, got %q", l.Protocol)
	}
	if l.TTFTMs < 10 {
		t.Fatalf("TTFT 应 >= 人为插入的 12ms 延迟, got %d", l.TTFTMs)
	}
	if l.DurationMs < l.TTFTMs {
		t.Fatalf("总耗时应 >= TTFT: duration=%d ttft=%d", l.DurationMs, l.TTFTMs)
	}
	if l.Note == "" {
		t.Fatal("Note 应给出人可读摘要(跳过/错误)")
	}
	// 旧字段兼容: duration_ms 载入时回填到 DurationMs
	old := `{"time":"2026-09-01T00:00:00Z","route":"zen","status":200,"duration_ms":1234}`
	if parsed := parseRequestLogsFromLines(old); len(parsed) != 1 || parsed[0].DurationMs != 1234 {
		t.Fatalf("旧记录 duration_ms 应兼容回填, got %+v", parsed)
	}
}

func TestProtocolFromPath(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "openai",
		"/v1/messages":         "anthropic",
		"/v1/responses":        "responses",
		"/admin/api/state":     "openai",
	}
	for path, want := range cases {
		if got := protocolFromPath(path); got != want {
			t.Fatalf("protocolFromPath(%q)=%q, want %q", path, got, want)
		}
	}
}

func TestTraceFromMissingContext(t *testing.T) {
	if tr := traceFrom(context.Background()); tr != nil {
		t.Fatal("无轨迹的 context 应返回 nil")
	}
	if id := reqIDFrom(context.Background()); id != "" {
		t.Fatalf("无轨迹时应返回空 id, got %q", id)
	}
}
