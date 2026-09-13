package app

import (
	"context"
	"errors"
	"testing"
)

// fakeNodeBox 是 nodeBoxInstance 的测试替身: 不实例化真实 sing-box, 仅记录
// Start/Close 是否被调用, 用于验证 failKeepOld 在构建失败时保留旧实例(§2.6 第 22 项)。
type fakeNodeBox struct {
	started bool
	closed  bool
}

func (f *fakeNodeBox) Start() error {
	f.started = true
	return nil
}

func (f *fakeNodeBox) Close() error {
	f.closed = true
	return nil
}

// TestSyncNodeBoxKeepsOldInstanceOnBuildFailure 验证 §2.6 第 22 项: 当实例构建失败时,
// nodeBox / nodePorts / catchAllPort 三项全局状态完全不变(保留上一个可用实例继续服务)。
//
// 通过注入 startNodeInstanceFn 返回错误来触发失败路径, 因此不需要真实 sing-box 实例化,
// 在 CLINE_PROXY_SKIP_NODEBOX=1 下也能跑(走注入的错误路径, 不碰真实 sing-box)。
func TestSyncNodeBoxKeepsOldInstanceOnBuildFailure(t *testing.T) {
	// 先建好"旧状态": 一个非 nil 的旧实例 + 一组出口端口 + 一个 catch-all 端口。
	oldBox := &fakeNodeBox{}
	oldPorts := map[string]int{"old-node.example.com:443": 15001}
	oldKeys := "old-node.example.com:443"
	oldCatchAll := 15000

	nodeMu.Lock()
	nodeBox = oldBox
	nodePorts = oldPorts
	nodePortsKeys = oldKeys
	catchAllPort = oldCatchAll
	nodeMu.Unlock()

	// 配置里无节点、无订阅, 让 buildNodeParts 不产生任何出站(从而不触碰真实 sing-box)。
	withTestConfig(t, &zenConfigData{})
	subMu.Lock()
	subNodes = nil
	subMu.Unlock()

	// 注入: 构建实例直接失败。这是触发 failKeepOld 的路径, 不需要真实 sing-box。
	// 走 setter 而不是直接给 var 赋值 —— setter 同时记下「已被注入」,
	// syncNodeBox 靠这个标记决定在 CLINE_PROXY_SKIP_NODEBOX 下是否放行注入路径。
	setStartNodeInstanceFn(func(ctx context.Context, inbounds, outbounds, rules []map[string]any) (nodeBoxInstance, error) {
		return nil, errors.New("injected build failure")
	})
	t.Cleanup(func() {
		setStartNodeInstanceFn(nil)
		nodeMu.Lock()
		nodeBox = nil
		nodePorts = nil
		nodePortsKeys = ""
		catchAllPort = 0
		nodeMu.Unlock()
	})

	syncNodeBox()

	// 三项全局状态必须与旧快照严格相等 —— 构建失败绝不能清空出口池或改动旧实例。
	nodeMu.Lock()
	gotBox := nodeBox
	gotPorts := nodePorts
	gotCatchAll := catchAllPort
	nodeMu.Unlock()

	if gotBox != nodeBoxInstance(oldBox) {
		t.Fatalf("failKeepOld 被破坏: nodeBox 被改动, 期望保留旧实例 %p, 实际 %p", oldBox, gotBox)
	}
	if len(gotPorts) != len(oldPorts) {
		t.Fatalf("failKeepOld 被破坏: nodePorts 长度变化 期望 %d, 实际 %d", len(oldPorts), len(gotPorts))
	}
	for k, v := range oldPorts {
		if gotPorts[k] != v {
			t.Fatalf("failKeepOld 被破坏: nodePorts[%q] 被改动 期望 %d 实际 %d", k, v, gotPorts[k])
		}
	}
	if gotCatchAll != oldCatchAll {
		t.Fatalf("failKeepOld 被破坏: catchAllPort 被改动 期望 %d 实际 %d", oldCatchAll, gotCatchAll)
	}
	// 旧实例绝不能被 Close —— 它还在继续服务。
	if oldBox.closed {
		t.Fatal("failKeepOld 被破坏: 旧实例在构建失败时不应被 Close")
	}
}
