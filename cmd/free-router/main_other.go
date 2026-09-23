//go:build !windows

package main

import "log"

// attachParentConsole 非 Windows 平台无需处理(控制台子系统本身可见)。
func attachParentConsole() {}

// msgboxFail 非 Windows 平台控制台日志可见, 直接 Fatal。
func msgboxFail(err error) {
	log.Fatalf("Proxy failed: %v", err)
}
