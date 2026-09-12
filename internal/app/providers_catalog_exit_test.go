package app

import (
	"testing"
)

// 目录抓取的换出口判定: 只有连接层错误与地区拒绝才换, 其余 HTTP 错误不换;
// 预算 = 池内**健康且未冷却**的出口数(冷却会逐步把失败出口移出可用集合,
// 因此轮换天然收敛), 另有 catalogExitMaxRotations 作为硬上限。
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

	t.Run("预算 = 池内可用出口数, 且轮换必然终止", func(t *testing.T) {
		proxies := []string{"http://127.0.0.1:19001", "http://127.0.0.1:19002", "http://127.0.0.1:19003"}
		withTestConfig(t, &zenConfigData{ExitMode: exitModeProxy, Proxies: proxies})
		budget := catalogExitBudget()
		if budget < 1 || budget > len(proxies) {
			t.Fatalf("预算 %d 应在 [1,%d] 内", budget, len(proxies))
		}
		p := newProvider()
		rotations := 0
		// 预算与硬上限共同保证终止: 预算随冷却递减, 且绝不超过 catalogExitMaxRotations
		for p.retryCatalogOnNextExit(errConnReset) {
			rotations++
			if rotations > catalogExitMaxRotations {
				t.Fatalf("轮换未收敛: 已换 %d 次, 硬上限 %d", rotations, catalogExitMaxRotations)
			}
		}
		if rotations == 0 {
			t.Fatal("池里有可用出口时必须允许换一次")
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
