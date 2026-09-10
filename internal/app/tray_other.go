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

// RunTray 非 Windows 平台暂不启用托盘,立即返回(服务已在后台运行)。
func RunTray(adminURL string) {}
