package app

// 出口级去重(同出口 IP 只留最快)的测试。

import (
	"testing"
	"time"
)

func seedHealth(key, exitIP string, lat int64) {
	nodeHealthMu.Lock()
	nodeHealth[key] = nodeHealthState{
		Ok:     true,
		At:     time.Now(),
		Result: nodeTestResult{Alive: true, ExitIP: exitIP, LatencyMs: lat},
	}
	nodeHealthMu.Unlock()
}

func TestExitFoldKeepsFastestPerExitIP(t *testing.T) {
	defer func() {
		nodeHealthMu.Lock()
		nodeHealth = map[string]nodeHealthState{}
		nodeHealthMu.Unlock()
		exitFoldMu.Lock()
		exitFoldDupOf = map[string]string{}
		exitFoldMu.Unlock()
	}()

	// a/b 同出口 IP(1.1.1.1): b 更快 → a 折叠到 b; c 独立出口不受影响
	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@fold-a.test:1", "1.1.1.1", 300)
	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@fold-b.test:1", "1.1.1.1", 80)
	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@fold-c.test:1", "2.2.2.2", 500)

	recomputeExitFold()

	if !nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@fold-a.test:1") {
		t.Fatal("慢的重复出口应被折叠")
	}
	if nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@fold-b.test:1") {
		t.Fatal("同组最快的应是主力, 不被折叠")
	}
	if nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@fold-c.test:1") {
		t.Fatal("独立出口不应被折叠")
	}
	exitFoldMu.RLock()
	best := exitFoldDupOf["ss://YWVzLTI1Ni1nY206cHdk@fold-a.test:1"]
	exitFoldMu.RUnlock()
	if best != "ss://YWVzLTI1Ni1nY206cHdk@fold-b.test:1" {
		t.Fatalf("折叠应指向组内最快者, got %q", best)
	}

	// 选路层: 折叠副本的 nodeUsable 应为 false
	if nodeUsable("ss://YWVzLTI1Ni1nY206cHdk@fold-a.test:1") {
		t.Fatal("折叠副本不应参与选路")
	}
	if !nodeUsable("ss://YWVzLTI1Ni1nY206cHdk@fold-b.test:1") {
		t.Fatal("主力应参与选路")
	}
}

func TestExitFoldReElectionOnDegrade(t *testing.T) {
	defer func() {
		nodeHealthMu.Lock()
		nodeHealth = map[string]nodeHealthState{}
		nodeHealthMu.Unlock()
		exitFoldMu.Lock()
		exitFoldDupOf = map[string]string{}
		exitFoldMu.Unlock()
	}()

	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@re-a.test:1", "3.3.3.3", 80) // 初代主力
	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@re-b.test:1", "3.3.3.3", 300)
	recomputeExitFold()
	if nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@re-a.test:1") || !nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@re-b.test:1") {
		t.Fatal("初始应 a 为主力")
	}

	// a 劣化(延迟变差): 下一轮折叠应改选 b
	seedHealth("ss://YWVzLTI1Ni1nY206cHdk@re-a.test:1", "3.3.3.3", 900)
	recomputeExitFold()
	if nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@re-b.test:1") {
		t.Fatal("主力劣化后 b 应转正")
	}
	if !nodeFoldedDuplicate("ss://YWVzLTI1Ni1nY206cHdk@re-a.test:1") {
		t.Fatal("劣化的 a 应被折叠")
	}
}
