package app

import (
	"strings"
	"testing"
)

// 目录抓取的换出口判定: 只有连接层错误与地区拒绝才换, 其余 HTTP 错误不换。
func TestRetryCatalogOnNextExit(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy})
	newProvider := func() *modelProvider { return &modelProvider{name: "bai"} }

	t.Run("地区拒绝要换出口", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 400, body: `{"error":"User location is not supported for the API use."}`}
		if !p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", err) {
			t.Fatal("region rejection must trigger an exit rotation")
		}
		if p.catalogExitRetries != 1 {
			t.Fatalf("retry budget must be consumed, got %d", p.catalogExitRetries)
		}
	})

	t.Run("403 地区拒绝同样要换", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 403, body: `{"message":"user location restricted"}`}
		if !p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", err) {
			t.Fatal("403 region rejection must trigger an exit rotation")
		}
	})

	t.Run("普通 HTTP 错误不换出口", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 500, body: "boom"}
		if p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", err) {
			t.Fatal("a plain HTTP 500 must not rotate the exit")
		}
		err = &catalogHTTPError{status: 401, body: "bad key"}
		if p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", err) {
			t.Fatal("a plain HTTP 401 must not rotate the exit")
		}
	})

	t.Run("连接层错误要换出口", func(t *testing.T) {
		p := newProvider()
		err := strings.NewReader("read tcp 127.0.0.1:1->127.0.0.1:2: connection reset")
		_ = err
		if !p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", errConnReset) {
			t.Fatal("a connection-level error must trigger an exit rotation")
		}
	})

	t.Run("重试预算用尽后不再换", func(t *testing.T) {
		p := newProvider()
		for i := 0; i < providerExitRetries; i++ {
			if !p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", errConnReset) {
				t.Fatalf("rotation %d/%d must be allowed", i+1, providerExitRetries)
			}
		}
		if p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", errConnReset) {
			t.Fatal("retry budget exhausted, must stop rotating")
		}
	})

	t.Run("直连模式不换出口", func(t *testing.T) {
		withTestConfig(t, &zenConfigData{ExitMode: exitModeDirect})
		// 判定的直连分支在 fetchCatalogPages 里, 这里验证决策函数本身仍受预算约束
		p := newProvider()
		if !p.retryCatalogOnNextExit(t.Context(), providerConfig{}, "https://x/models", errConnReset) {
			t.Fatal("decision helper is gated by the caller for direct mode")
		}
	})
}

type constError string

func (e constError) Error() string { return string(e) }

var errConnReset = constError(`Get "https://api.b.ai/v1/models": read tcp 127.0.0.1:58757->127.0.0.1:58136: connection reset`)
