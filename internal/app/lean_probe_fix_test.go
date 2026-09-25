package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 6.1 Ok 只看 Alive(+country 未知不判死): 质量项 MITM/断流/WARP/低速/机房
// 再差也不能把可用打成不可用; Alive=false 则一律不可用。
func TestLeanVerdictIgnoresQuality(t *testing.T) {
	bad := nodeTestResult{Alive: true, ExitCountry: "US", MITMRisk: true, IsStalled: true, IsWarp: true, SpeedBPS: 1, NetworkType: "datacenter"}
	if !leanVerdict(bad) {
		t.Fatalf("Alive=true 即使质量全差也必须判可用, got false: %+v", bad)
	}
	unknown := nodeTestResult{Alive: true, MITMRisk: true, IsStalled: true}
	if !leanVerdict(unknown) {
		t.Fatalf("Alive=true + 国家未知必须判可用(未知≠不可用), got false")
	}
	dead := nodeTestResult{Alive: false, ExitCountry: "US"}
	if leanVerdict(dead) {
		t.Fatalf("Alive=false 必须判不可用")
	}
}

// 6.2 opencode.ai 可达门 + 单源首胜: 网关不可达直接判死, 不再跑后续;
// 多源并行时首个成功即返回, 不等慢源。
func TestOpencodeReachableGateSingleFastest(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer slow.Close()

	start := time.Now()
	ok := probeNodeGatewayReachable(&http.Client{Timeout: 30 * time.Second}, []string{fast.URL, slow.URL})
	if !ok {
		t.Fatalf("首源 204 必须判可达")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("单源首胜不应等慢源: %v", elapsed)
	}

	dead := probeNodeGatewayReachable(&http.Client{Timeout: 30 * time.Second}, []string{"http://127.0.0.1:1/zzz"})
	if dead {
		t.Fatalf("网关全不可达必须判死")
	}
}

// 6.4 omitempty: 零值质量字段不得落盘, 旧 node-health.json(无这些键)必须可载。
func TestNodeTestResultOmitemptyCompat(t *testing.T) {
	raw, err := json.Marshal(nodeTestResult{Alive: true, ExitCountry: "US"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, k := range []string{"speedBPS", "isStalled", "mitmRisk", "isWarp", "SpeedBPS", "IsStalled", "MITMRisk", "IsWarp"} {
		if strings.Contains(s, `"`+k+`"`) {
			t.Fatalf("零值质量字段必须 omitempty, JSON 不应含 %s: %s", k, s)
		}
	}
	var back nodeTestResult
	if err := json.Unmarshal([]byte(`{"Alive":true,"ExitCountry":"JP"}`), &back); err != nil {
		t.Fatalf("旧缓存无质量键必须可载: %v", err)
	}
	if !back.Alive || back.ExitCountry != "JP" {
		t.Fatalf("旧缓存字段必须保留: %+v", back)
	}
}

// 7.1 计数 dead/alive/country-unknown: 速度/慢项不再单列。
func TestHealthCountersDeadAliveUnknown(t *testing.T) {
	rs := []nodeTestResult{
		{Alive: false},
		{Alive: true, ExitCountry: "US"},
		{Alive: true},
	}
	dead, alive, unknown := summarizeProbeVerdicts(rs)
	if dead != 1 || alive != 2 || unknown != 1 {
		t.Fatalf("dead/alive/unknown 应为 1/2/1, got %d/%d/%d", dead, alive, unknown)
	}
}

// 8.1 host:port 复用零网络 + 快车道并发上限 8。
func TestHostPortReuseZeroNetwork(t *testing.T) {
	if fastLaneMaxWorkers != 8 {
		t.Fatalf("快车道并发上限必须为 8, got %d", fastLaneMaxWorkers)
	}
	prevFn := testNodeComprehensiveFn
	called := 0
	testNodeComprehensiveFn = func(key string) nodeTestResult { called++; return nodeTestResult{Alive: true} }
	t.Cleanup(func() { testNodeComprehensiveFn = prevFn })

	rememberNodeCountry("seed-key", "US")
	setNodeRemoteEndpoints(map[string]string{"seed-key": "10.9.9.9:443", "new-key": "10.9.9.9:443"})
	t.Cleanup(func() { setNodeRemoteEndpoints(map[string]string{}) })

	if n := seedNewNodeCountries([]string{"seed-key", "new-key"}); n < 1 {
		t.Fatalf("同 host:port 新键应被继承, got %d", n)
	}
	if called != 0 {
		t.Fatalf("host:port 复用必须零网络探测, 实际探测 %d 次", called)
	}
	if got := rememberedNodeCountry("new-key"); got != "US" {
		t.Fatalf("new-key 应继承 US, got %q", got)
	}
}
