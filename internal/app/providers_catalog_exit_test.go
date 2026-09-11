package app

import (
	"testing"
)

// 目录抓取的换出口判定: 只有连接层错误与地区拒绝才换, 其余 HTTP 错误不换;
// 预算 = 节点池出口数, 每个节点各试一次。
func TestRetryCatalogOnNextExit(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy})
	newProvider := func() *modelProvider { return &modelProvider{name: "bai"} }

	t.Run("地区拒绝要换出口", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 400, body: `{"error":"User location is not supported for the API use."}`}
		if !p.retryCatalogOnNextExit(err) {
			t.Fatal("region rejection must trigger an exit rotation")
		}
		if p.catalogExitRetries != 1 {
			t.Fatalf("retry budget must be consumed, got %d", p.catalogExitRetries)
		}
	})

	t.Run("403 地区拒绝同样要换", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 403, body: `{"message":"user location restricted"}`}
		if !p.retryCatalogOnNextExit(err) {
			t.Fatal("403 region rejection must trigger an exit rotation")
		}
	})

	t.Run("普通 HTTP 错误不换出口", func(t *testing.T) {
		p := newProvider()
		err := &catalogHTTPError{status: 500, body: "boom"}
		if p.retryCatalogOnNextExit(err) {
			t.Fatal("a plain HTTP 500 must not rotate the exit")
		}
		err = &catalogHTTPError{status: 401, body: "bad key"}
		if p.retryCatalogOnNextExit(err) {
			t.Fatal("a plain HTTP 401 must not rotate the exit")
		}
	})

	t.Run("连接层错误要换出口", func(t *testing.T) {
		p := newProvider()
		if !p.retryCatalogOnNextExit(errConnReset) {
			t.Fatal("a connection-level error must trigger an exit rotation")
		}
	})

	t.Run("预算等于节点池出口数", func(t *testing.T) {
		proxies := []string{"http://127.0.0.1:19001", "http://127.0.0.1:19002", "http://127.0.0.1:19003"}
		withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, Proxies: proxies})
		if got := catalogExitBudget(); got != len(proxies) {
			t.Fatalf("budget must equal pool size %d, got %d", len(proxies), got)
		}
		p := newProvider()
		for i := 0; i < len(proxies); i++ {
			if !p.retryCatalogOnNextExit(errConnReset) {
				t.Fatalf("rotation %d/%d must be allowed", i+1, len(proxies))
			}
		}
		if p.retryCatalogOnNextExit(errConnReset) {
			t.Fatal("every node tried, must stop rotating")
		}
	})

	t.Run("空池预算为 1 保证可终止", func(t *testing.T) {
		withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy})
		if got := catalogExitBudget(); got != 1 {
			t.Fatalf("empty pool budget must be 1, got %d", got)
		}
	})
}

type constError string

func (e constError) Error() string { return string(e) }

var errConnReset = constError(`Get "https://api.b.ai/v1/models": read tcp 127.0.0.1:58757->127.0.0.1:58136: connection reset`)
