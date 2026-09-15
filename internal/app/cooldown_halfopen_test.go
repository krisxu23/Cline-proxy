package app

// 冷却半开探测 (P2-23) 的测试: 到期→探针→痊愈/加倍 的完整生命周期。

import (
	"strings"
	"testing"
	"time"
)

func TestHalfOpenProbeLifecycle(t *testing.T) {
	const up, m = "halftest", "model-x"
	defer func() {
		candidateCoolMu.Lock()
		delete(candidateCools, candidateKey(up, m))
		candidateCoolMu.Unlock()
	}()

	// 1) 进入冷却(serverError 默认 2 分钟)
	markCandidateCooldown(up, m, classServerError, "first failure")
	if candidateSkipReason(up, m) == "" {
		t.Fatal("冷却期内应被跳过")
	}

	// 2) 时间快进: 把 until 拨到过去(测试无法等 2 分钟)
	candidateCoolMu.Lock()
	k := candidateKey(up, m)
	c := candidateCools[k]
	c.until = time.Now().UnixMilli() - 1
	candidateCools[k] = c
	candidateCoolMu.Unlock()

	// 3) 到期后第一次询问: 应放行(转为探测态), 不再有"冷却中"原因
	if why := candidateSkipReason(up, m); why != "" {
		t.Fatalf("到期后应放行探针, got %q", why)
	}
	// 探测态再次询问仍放行(并发请求都可成为探针)
	if why := candidateSkipReason(up, m); why != "" {
		t.Fatalf("探测态应继续放行, got %q", why)
	}

	// 4) 探针失败 → 重新冷却, 时长加倍(2min → 4min)
	markCandidateCooldown(up, m, classServerError, "probe failed")
	candidateCoolMu.Lock()
	c2 := candidateCools[k]
	remaining := c2.until - time.Now().UnixMilli()
	fails := c2.probeFails
	candidateCoolMu.Unlock()
	if fails != 1 {
		t.Fatalf("探测失败计数应为 1, got %d", fails)
	}
	if remaining < int64(3*60*1000) || remaining > int64(4*60*1000) {
		t.Fatalf("探测失败后冷却应加倍到约 4 分钟, got %d ms", remaining)
	}
	if !strings.Contains(c2.reason, "探测失败1次") {
		t.Fatalf("原因串应带探测次数, got %q", c2.reason)
	}

	// 5) 探针成功 → 彻底清除, 恢复健康
	markCandidateSuccess(up, m)
	if why := candidateSkipReason(up, m); why != "" {
		t.Fatalf("成功后应恢复健康, got %q", why)
	}
}
