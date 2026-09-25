package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Task1.1: 冷启动空池时 zen/region 受限路径绝不能静默直连污染 h2 池。
// 期望: 空池 + region 受限模型 → 503 retryable 或先走 syncNodeBox/loadSubCache 重建,
// 绝不能直接返回直连出口 ("", -1) 就发请求。
func TestChainColdStartNoDirectPollution(t *testing.T) {
	no := false
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, RescueDirect: &no, Subs: nil, Proxies: nil})
	resetNodeBoxForTest(t)
	syncNodeBox()
	// 确保订阅快照也为空
	if n := len(effectiveProxyList()); n != 0 {
		t.Skipf("pool not empty in this env (%d), skip cold-start assertion", n)
	}
	modelID := "muse-spark-1.3-contributor-free"
	markModelRegionRestricted(modelID)
	// 门禁必须拦截: 空池 + 受限模型不允许 direct
	if gateAllowsDirectForModel(modelID) {
		t.Fatalf("cold-start empty pool: region-restricted %s must NOT allow silent direct (h2 pollution)", modelID)
	}
	// HTTP 层必须回 503 retryable JSON, 不能悄悄发上游
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	tr := &reqTrace{RequestID: "req-test-cold"}
	req = req.WithContext(withReqTrace(req.Context(), tr))
	w := httptest.NewRecorder()
	if !writePoolEmptyGate(w, req, modelID) {
		t.Fatalf("writePoolEmptyGate must handle empty-pool restricted model")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("gate must return 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "retryable") {
		t.Fatalf("gate 503 must carry retryable flag, got %s", w.Body.String())
	}
}

// Task1.2: gate region 受限模型当池空 (503+retryable JSON 带 error code);
// unrestricted 才允许 direct 并打明确日志。
func TestChainGateRegionRestrictedVsUnrestricted(t *testing.T) {
	no := false
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, RescueDirect: &no, Proxies: nil})
	resetNodeBoxForTest(t)
	syncNodeBox()
	if n := len(effectiveProxyList()); n != 0 {
		t.Skipf("pool not empty (%d), skip gate assertion", n)
	}
	restricted := "muse-spark-1.3-contributor-free"
	markModelRegionRestricted(restricted)
	if gateAllowsDirectForModel(restricted) {
		t.Fatalf("restricted model %s must be gated when pool empty", restricted)
	}
	// unrestricted 模型允许 direct
	if !gateAllowsDirectForModel("mimo-v2.5-free") {
		t.Fatalf("unrestricted model must allow direct when pool empty")
	}
	// gate 响应必须带 error code
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(withReqTrace(req.Context(), &reqTrace{RequestID: "req-gate"}))
	w := httptest.NewRecorder()
	writePoolEmptyGate(w, req, restricted)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("restricted gate must be 503, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "retryable") || !strings.Contains(body, "code") {
		t.Fatalf("gate 503 must carry retryable + error code, got %s", body)
	}
}

// Task1.3: 链失败路径必须填充 reqTrace tried/skipped + error class, 无静默丢弃。
func TestChainTraceNoSilentDrop(t *testing.T) {
	withTestConfig(t, &zenConfigData{
		Routes: map[string][]string{"trace-chain": {"ghost:m1", "ghost2:m2"}},
	})
	resetCandidateState()
	defer resetCandidateState()
	tr := &reqTrace{RequestID: "req-trace-test"}
	ctx := withReqTrace(context.Background(), tr)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	chain := []routeCandidate{{Upstream: "ghost", Model: "m1"}, {Upstream: "ghost2", Model: "m2"}}
	handleChainedChat(w, req, map[string]any{"model": "trace-chain"}, chain, "trace-chain")
	if w.Code < 400 {
		t.Fatalf("unknown providers must fail the chain, got %d", w.Code)
	}
	snap := tr.snapshot()
	if len(snap.Skipped) == 0 {
		t.Fatalf("skipped candidates must be recorded in reqTrace, got %+v", snap)
	}
	if snap.ErrClass == "" {
		t.Fatalf("chain failure must set reqTrace ErrClass, got %+v", snap)
	}
	d := decisionTraceFor("req-trace-test")
	if d == nil || len(d.Candidates) == 0 {
		t.Fatalf("decision trace must record candidates, got %+v", d)
	}
}
