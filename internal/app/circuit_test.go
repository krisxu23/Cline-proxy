package app

import (
	"testing"
	"time"
)

// resetZenCircuit 归零熔断状态, 避免用例间串扰。
func resetZenCircuit() {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenProbing = false
	zenStateMu.Unlock()
}

func TestZenCircuitBreakerProbeRetrips(t *testing.T) {
	resetZenCircuit()
	defer resetZenCircuit()

	// 进入熔断: 连续失败达到阈值
	for i := 0; i < 3; i++ {
		markZenFail()
	}
	if !zenFailedNow() {
		t.Fatal("达到阈值后应处于熔断状态")
	}

	// 窗口过期: 放行半开探测
	zenStateMu.Lock()
	zenFailUntil = time.Now().Add(-time.Minute)
	zenStateMu.Unlock()
	if zenFailedNow() {
		t.Fatal("窗口过期应放行探测请求")
	}

	// 探测失败: 立即重新跳闸, 无需重新累计 3 次
	markZenFail()
	if !zenFailedNow() {
		t.Fatal("探测失败应立即重新进入熔断状态")
	}

	// 窗口再次过期后探测成功: 熔断完全恢复
	zenStateMu.Lock()
	zenFailUntil = time.Now().Add(-time.Minute)
	zenStateMu.Unlock()
	zenFailedNow() // 放行探测
	markZenSuccess()
	if zenFailedNow() {
		t.Fatal("探测成功后应恢复放行")
	}
}

func TestZenCircuitBreakerClientErrorsDoNotTrip(t *testing.T) {
	resetZenCircuit()
	defer resetZenCircuit()

	// 400/401/404 属于模型或请求问题, 不触发熔断
	for _, status := range []int{400, 401, 404} {
		markZenFailOnStatus(status)
	}
	if zenFailedNow() {
		t.Fatal("客户端类错误不应触发熔断")
	}

	// 429/408/5xx 代表上游不可用, 计入熔断
	for _, status := range []int{429, 408, 500, 502, 503} {
		resetZenCircuit()
		markZenFailOnStatus(status)
		markZenFailOnStatus(status)
		markZenFailOnStatus(status)
		if !zenFailedNow() {
			t.Fatalf("status %d 连续三次应触发熔断", status)
		}
	}
}
