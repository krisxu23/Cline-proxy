//go:build !windows

package app

import (
	"os/exec"
	"runtime"
)

// OpenAdminWindow 非 Windows 平台回退到默认浏览器。
func OpenAdminWindow(adminURL string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", adminURL).Start()
	default:
		exec.Command("xdg-open", adminURL).Start()
	}
}

// RunTray 非 Windows 没有托盘图标: 必须阻塞等根上下文取消(退出信号或显式
// Shutdown), 否则 runDesktop 立即返回、main 退出, 后台服务随之被杀 —— 与
// Windows 托盘"阻塞到用户退出"的语义对齐。返回前走与托盘退出相同的收口
// (doGracefulShutdown 由 sync.Once 保证只跑一次, 与信号 watcher 并发时
// 阻塞到对方完成), 排空在途请求后进程才真正退出。
func RunTray(adminURL string) {
	<-appRootCtx.Done()
	doGracefulShutdown()
}
