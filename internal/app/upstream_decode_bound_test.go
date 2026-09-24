package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpstreamDecodeAlwaysBounded 源码层锁: 上游响应体的解码**必须封顶**。
//
// 背景(2026-09-24 审计 P0-2): 上游响应体完全由不受我们控制的一方决定 —— zen 侧还要
// 经第三方免费订阅出口节点那一跳, 代码自己的健康探测就在查 MITM。裸
// `json.NewDecoder(resp.Body).Decode(...)` 会边读边分配, 一个不封顶的响应就能把网关
// 进程吃成 OOM(连带正在跑的 agent 会话一起挂)。审计时这样的散点有 15 处
// (app 侧 11 处 + cline/auth.go 4 处), 现已统一收敛到 `kit.DecodeJSONLimit`。
//
// 为什么用**源码层**锁而不是逐个调用点的行为测试: 这条不变量是"全仓扫描"性质的 ——
// 真正的风险是**将来新增**一处忘了封顶, 而行为测试只能覆盖已写的那几处。同仓已有
// 先例(见 claude_tool_remap_wiring_test.go 的"源码层接线锁")。
//
// 扫描面: internal/app 与 internal/cline 两个包的**全部非测试 .go 文件**(不写死文件
// 清单, 这样新增文件也会被扫到)。
func TestUpstreamDecodeAlwaysBounded(t *testing.T) {
	// 未封顶的写法: 直接把上游 body 交给 Decoder。封顶写法是
	// kit.DecodeJSONLimit(resp.Body, ...)(内部套 LimitedReader), 因此不会命中。
	unbounded := []string{
		"json.NewDecoder(resp.Body)",
		"json.NewDecoder(up.Body)",
		"json.NewDecoder(rc.Body)",
	}
	for _, dir := range []string{".", "../cline"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读取目录 %s 失败: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("读取 %s 失败: %v", name, err)
			}
			src := string(b)
			for _, bad := range unbounded {
				if strings.Contains(src, bad) {
					t.Errorf("%s/%s 出现无界上游解码 %q —— 必须走 kit.DecodeJSONLimit(见其注释)",
						dir, name, bad)
				}
			}
		}
	}
}
