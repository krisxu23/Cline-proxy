package app

import "testing"

// requireNodeBox 依赖真实 sing-box 实例的用例, 在测试进程被要求跳过实例化时跳过。
//
// 这几个用例验证的是端到端行为 —— 直连也经 sing-box 的 catch-all 入站、
// 节点桥接真的能把流量转发出去、全部出站类型都能被实例化 —— 都必须靠真实
// 实例, 无法用替身。
//
// 触发条件见 nodeBoxSkipRequested(): 测试二进制 + 显式环境变量。CI 的 -race
// 任务会带上它, 因为 sing-box 自身的后台 goroutine 之间存在数据竞争(第三方
// 内部问题), 会把竞态检测任务染红。普通测试任务不带该变量, 这些用例照常运行。
func requireNodeBox(t *testing.T) {
	t.Helper()
	if nodeBoxSkipRequested() {
		t.Skip("需要真实 sing-box 实例; 当前测试进程要求跳过实例化(见 CLINE_PROXY_SKIP_NODEBOX), 该用例只在普通构建下运行")
	}
}
