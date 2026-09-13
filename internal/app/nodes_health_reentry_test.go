package app

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckAllNodeHealthReentryCompensates 锁住 §2.6 第 23 项:
//
//	"重入即丢弃"必须补偿 —— 第一轮还在跑时又有人触发, 置 healthRunAgain,
//	第一轮结束后立刻补跑一次。
//
// 为什么要专门测: 报告 §4 确认过这段逻辑本身没有数据竞争(读写都在
// nodeHealthRunMu 内, defer 先置 nodeHealthRunning=false 再读标记), 所以这里
// 不是为了"找 bug", 而是把正确行为钉死, 防止日后有人把 defer 里的
// `again := healthRunAgain` 改成裸读、或把递归补跑删掉时测试能立刻发现。
//
// 不测真实网络探测: 节点连通检测要走真实出口链路, 改成注入 testNodeComprehensiveFn
// 替身, 用 channel 当门闩控制时序。
func TestCheckAllNodeHealthReentryCompensates(t *testing.T) {
	// ---- 保存并重置全局状态 ----
	nodeMu.Lock()
	prevPorts := nodePorts
	nodePorts = map[string]int{"n-reentry-1": 1}
	nodeMu.Unlock()

	nodeHealthMu.Lock()
	prevHealth := nodeHealth
	nodeHealth = map[string]nodeHealthState{}
	nodeHealthMu.Unlock()

	nodeHealthRunMu.Lock()
	prevRunning := nodeHealthRunning
	prevAgain := healthRunAgain
	nodeHealthRunning = false
	healthRunAgain = false
	nodeHealthRunMu.Unlock()

	// 探测替身恢复
	prevFn := testNodeComprehensiveFn
	calls := int32(0)
	gate := make(chan struct{})     // 替身第一轮在此等待
	released := make(chan struct{}) // 第一轮首次被调用时关闭, 通知主流程
	var firstClosed int32
	testNodeComprehensiveFn = func(key string) nodeTestResult {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			if atomic.CompareAndSwapInt32(&firstClosed, 0, 1) {
				close(released)
			}
			// 第一轮: 卡住, 模拟"上百节点探测还在跑"
			<-gate
		}
		// 第二轮(及以后): 立刻返回, 避免补跑再卡住
		return nodeTestResult{Alive: true}
	}
	t.Cleanup(func() {
		testNodeComprehensiveFn = prevFn
		nodeMu.Lock()
		nodePorts = prevPorts
		nodeMu.Unlock()
		nodeHealthMu.Lock()
		nodeHealth = prevHealth
		nodeHealthMu.Unlock()
		nodeHealthRunMu.Lock()
		nodeHealthRunning = prevRunning
		healthRunAgain = prevAgain
		nodeHealthRunMu.Unlock()
	})

	// ---- 第一轮: 在独立 goroutine 里跑, 让它卡住 ----
	done := make(chan struct{})
	go func() {
		checkAllNodeHealth()
		close(done)
	}()

	// 等第一轮真的开始探测(还没释放 gate)
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有在超时前进入探测阶段")
	}

	// ---- 重入: 第一轮仍在跑, 再触发一次 ----
	checkAllNodeHealth()

	nodeHealthRunMu.Lock()
	againAfterReentry := healthRunAgain
	runningAfterReentry := nodeHealthRunning
	nodeHealthRunMu.Unlock()
	if !runningAfterReentry {
		t.Fatalf("重入时 nodeHealthRunning 应为 true(第一轮还在跑)")
	}
	if !againAfterReentry {
		t.Fatalf("重入未置 healthRunAgain —— 这正是被丢的补偿标记")
	}

	// ---- 放行第一轮 ----
	close(gate)
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("第一轮结束后补跑没有完成(可能卡在补跑轮)")
	}

	// ---- 关键断言: 补跑轮真的跑了 ----
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("探测被调用 %d 次, 期望 2(第一轮 1 次 + 补跑 1 次)", n)
	}

	// 补跑结束, 标记必须被清掉, 否则会误触发下一轮空转
	nodeHealthRunMu.Lock()
	running, again := nodeHealthRunning, healthRunAgain
	nodeHealthRunMu.Unlock()
	if running {
		t.Fatalf("结束后 nodeHealthRunning 仍为 true(死锁等待点)")
	}
	if again {
		t.Fatalf("结束后 healthRunAgain 仍为 true(会触发多余补跑)")
	}

	// 第一轮探测结果必须真的落进 nodeHealth
	nodeHealthMu.Lock()
	defer nodeHealthMu.Unlock()
	st, ok := nodeHealth["n-reentry-1"]
	if !ok {
		t.Fatal("补跑轮没有写入 nodeHealth")
	}
	if !st.Ok {
		t.Fatalf("节点健康结果应为可达(替身返回 Alive=true): %+v", st)
	}
}

// TestCheckAllNodeHealthEmptyPoolSkips 出口池为空时跳过本轮且明确告警,
// 不留半初始化状态 —— 与上面的补跑用例互为边界: 空池不应无限递归补跑。
func TestCheckAllNodeHealthEmptyPoolSkips(t *testing.T) {
	nodeMu.Lock()
	prev := nodePorts
	nodePorts = map[string]int{}
	nodeMu.Unlock()
	t.Cleanup(func() {
		nodeMu.Lock()
		nodePorts = prev
		nodeMu.Unlock()
	})

	nodeHealthRunMu.Lock()
	prevR, prevA := nodeHealthRunning, healthRunAgain
	nodeHealthRunning = false
	healthRunAgain = false
	nodeHealthRunMu.Unlock()
	t.Cleanup(func() {
		nodeHealthRunMu.Lock()
		nodeHealthRunning = prevR
		healthRunAgain = prevA
		nodeHealthRunMu.Unlock()
	})

	// 空池也必须能正常结束, 不能把 nodeHealthRunning 卡成 true
	checkAllNodeHealth()

	nodeHealthRunMu.Lock()
	running := nodeHealthRunning
	again := healthRunAgain
	nodeHealthRunMu.Unlock()
	if running {
		t.Fatalf("空池返回后 nodeHealthRunning 仍为 true")
	}
	if again {
		t.Fatalf("空池不该置 healthRunAgain")
	}
}
