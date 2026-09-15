package app

// 空入口守卫的测试 (P2 修复): 订阅刷新中间态的"0 节点解析结果"不得顶掉
// 正在服务的实例 —— 实测事故: 0 出口实例上线后探测循环跳过, 面板全部 0 可用。

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestSyncNodeBoxKeepsInstanceWhenParseEmptyButSourcesRemain(t *testing.T) {
	// 旧状态: 一个 4487 出口级别的健康实例(用 2 个出口代表)
	oldBox := &fakeNodeBox{}
	oldPorts := map[string]int{
		"a.example.com:443": 16001,
		"b.example.com:443": 16002,
	}
	nodeMu.Lock()
	nodeBox = oldBox
	nodePorts = oldPorts
	nodePortsKeys = "a.example.com:443|b.example.com:443"
	catchAllPort = 16000
	nodeMu.Unlock()

	// 配置: 订阅源仍在(用户没清空), 但 subNodes 解析结果为空(刷新中间态)
	withTestConfig(t, &zenConfigData{Subs: []string{"https://example.com/sub"}})
	subMu.Lock()
	subNodes = nil
	subMu.Unlock()

	var calls atomic.Int32
	setStartNodeInstanceFn(func(ctx context.Context, inbounds, outbounds, rules []map[string]any) (nodeBoxInstance, error) {
		calls.Add(1)
		return &fakeNodeBox{}, nil
	})
	t.Cleanup(func() {
		setStartNodeInstanceFn(nil)
		nodeMu.Lock()
		nodeBox = nil
		nodeMu.Unlock()
	})

	syncNodeBox()

	if calls.Load() != 0 {
		t.Fatalf("空解析不应触发重建(实例启动被调用 %d 次)", calls.Load())
	}
	nodeMu.Lock()
	sameBox := nodeBox == oldBox
	samePorts := len(nodePorts) == 2
	nodeMu.Unlock()
	if !sameBox || !samePorts {
		t.Fatalf("原实例与出口必须原样保留: sameBox=%v samePorts=%v", sameBox, samePorts)
	}
}

func TestSyncNodeBoxAllowsGenuineEmptySources(t *testing.T) {
	// 来源真的清空(无手动节点无订阅): 允许 0 出口实例(直连也要过 sing-box)
	oldBox := &fakeNodeBox{}
	nodeMu.Lock()
	nodeBox = oldBox
	nodePorts = map[string]int{"a.example.com:443": 16003}
	nodePortsKeys = "a.example.com:443"
	catchAllPort = 16000
	nodeMu.Unlock()

	withTestConfig(t, &zenConfigData{})
	subMu.Lock()
	subNodes = nil
	subMu.Unlock()

	setStartNodeInstanceFn(func(ctx context.Context, inbounds, outbounds, rules []map[string]any) (nodeBoxInstance, error) {
		// 用户主动清空: 应允许重建(这里返回一个新实例验证确实重建了)
		return &fakeNodeBox{}, nil
	})
	t.Cleanup(func() {
		setStartNodeInstanceFn(nil)
		nodeMu.Lock()
		nodeBox = nil
		nodeMu.Unlock()
	})

	syncNodeBox()
	nodeMu.Lock()
	rebuilt := nodeBox != oldBox
	nodeMu.Unlock()
	if !rebuilt {
		t.Fatal("来源真清空时应允许重建 0 出口实例")
	}
}
