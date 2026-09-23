package app

// 健康表落盘(2026-09-23): 修"重启/订阅重建后 ~2.5 分钟内全部节点被当成可用"。
//
// 背景实测: 每次节点池重建后, 到首轮全量探测完成有约 2 分半的窗口
// (12s 启动延迟 + 约 2.5min 探测), 期间 healthOf 对每个节点返回 "unknown",
// 而 nodeUsable 的口径是"未检测 ≠ 不可用" —— 池子里约 85% 的死节点(1131/1327)
// 全部进入候选集。落盘健康结论后, 重启即可恢复上次的可用/不可用判定。

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// withTestNodeHealthFile 把健康表落盘路径指向临时文件, 并清空内存表。
func withTestNodeHealthFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/node-health.json"
	prevPath := nodeHealthFileOverride
	nodeHealthFileOverride = path

	nodeHealthMu.Lock()
	prev := nodeHealth
	nodeHealth = map[string]nodeHealthState{}
	nodeHealthMu.Unlock()

	t.Cleanup(func() {
		nodeHealthFileOverride = prevPath
		nodeHealthMu.Lock()
		nodeHealth = prev
		nodeHealthMu.Unlock()
	})
	return path
}

// 落盘 → 读回: 健康结论(含 ExitIP/延迟等折叠与选路要用的字段)必须完整往返。
func TestNodeHealthPersistRoundTrip(t *testing.T) {
	path := withTestNodeHealthFile(t)

	nodeHealthMu.Lock()
	nodeHealth["node-a"] = nodeHealthState{
		Ok: true, At: time.Now(),
		Result: nodeTestResult{Alive: true, LatencyMs: 123, ExitIP: "203.0.113.7", ExitCountry: "US"},
	}
	nodeHealth["node-b"] = nodeHealthState{Ok: false, At: time.Now()}
	nodeHealthMu.Unlock()

	persistNodeHealth()

	// 落盘后清空内存表, 模拟重启。
	nodeHealthMu.Lock()
	nodeHealth = map[string]nodeHealthState{}
	nodeHealthMu.Unlock()

	loadNodeHealthCache()

	if got := healthOf("node-a"); got != "ok" {
		t.Fatalf("可用节点应恢复为 ok, got %q", got)
	}
	if got := healthOf("node-b"); got != "fail" {
		t.Fatalf("不可用节点应恢复为 fail(这正是本机制要修的关键点), got %q", got)
	}
	nodeHealthMu.RLock()
	r := nodeHealth["node-a"].Result
	nodeHealthMu.RUnlock()
	if r.ExitIP != "203.0.113.7" || r.LatencyMs != 123 {
		t.Fatalf("出口信息应完整往返(折叠表依赖 ExitIP), got %+v", r)
	}
	_ = path
}

// 过期结论必须丢弃: 网关停了很久再开, 旧结论没有参考价值, 让它等首轮探测,
// 而不是拿一周前的"可用"去选路。
func TestNodeHealthCacheDropsStaleEntries(t *testing.T) {
	path := withTestNodeHealthFile(t)

	stale := map[string]nodeHealthState{
		"old-ok":   {Ok: true, At: time.Now().Add(-nodeHealthCacheTTL - time.Hour)},
		"fresh-ok": {Ok: true, At: time.Now()},
	}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loadNodeHealthCache()

	if got := healthOf("fresh-ok"); got != "ok" {
		t.Fatalf("未过期结论应恢复, got %q", got)
	}
	if got := healthOf("old-ok"); got != "unknown" {
		t.Fatalf("过期结论应丢弃回到未检测, got %q", got)
	}
}

// 加载发生在首轮探测之后时, 不能用磁盘上的旧结论覆盖本进程刚测出的新结论。
func TestNodeHealthCacheNeverOverwritesFreshResult(t *testing.T) {
	path := withTestNodeHealthFile(t)

	// 磁盘上写着"可用", 但本进程刚测出它挂了。
	b, err := json.Marshal(map[string]nodeHealthState{"node-x": {Ok: true, At: time.Now()}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	nodeHealthMu.Lock()
	nodeHealth["node-x"] = nodeHealthState{Ok: false, At: time.Now()}
	nodeHealthMu.Unlock()

	loadNodeHealthCache()

	if got := healthOf("node-x"); got != "fail" {
		t.Fatalf("新结论必须胜出, got %q", got)
	}
}
