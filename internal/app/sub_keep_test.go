package app

import (
	"testing"
)

// 回归: 全部订阅抓取失败时必须保留原有节点。
// 出口池全靠订阅供给, 一次网络抖动曾把 subNodes 连同订阅缓存一起清空 ——
// 面板显示"暂无出口节点"、请求全部失败, 连重启都救不回来。
func TestResolveSubscriptionsKeepsNodesWhenAllFetchesFail(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeDirect})
	subMu.Lock()
	subNodes = []any{"vless://keep-1", "vless://keep-2"}
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = nil
		subMu.Unlock()
	})

	// 127.0.0.1:1 立即拒绝连接, 两条路径(出口/直连兜底)都会快速失败
	resolveSubscriptions([]string{"http://127.0.0.1:1/sub"})

	subMu.Lock()
	got := len(subNodes)
	subMu.Unlock()
	if got != 2 {
		t.Fatalf("全部抓取失败时必须保留原有 2 个节点, 现有 %d 个", got)
	}
}

// 用户删空订阅列表是明确意图: 此时才应该清空节点。
func TestResolveSubscriptionsClearsWhenListEmptied(t *testing.T) {
	withTestConfig(t, &zenConfigData{ExitMode: exitModeDirect})
	subMu.Lock()
	subNodes = []any{"vless://keep-1"}
	subMu.Unlock()
	t.Cleanup(func() {
		subMu.Lock()
		subNodes = nil
		subMu.Unlock()
	})

	resolveSubscriptions(nil)

	subMu.Lock()
	got := len(subNodes)
	subMu.Unlock()
	if got != 0 {
		t.Fatalf("清空订阅列表后节点应为 0, 现有 %d 个", got)
	}
}
