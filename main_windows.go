//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
	"unsafe"
)

// attachParentConsole 把 GUI 子系统(-H=windowsgui)的进程重新挂回父控制台,
// 使 -list/-login/-start 等 CLI 模式在终端里能正常输出。
// 仅当原 stdout 句柄无效(终端直跑的 GUI 进程拿不到控制台句柄)时才覆盖;
// stdout 已被重定向到管道/文件时保留原句柄, 保证 `exe -list | ...` 可用。
// 双击启动(无父控制台)时挂接失败,静默忽略,不影响桌面模式。
func attachParentConsole() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	// stdout 已是控制台或已被重定向(管道/文件)时, 保留原句柄不动
	getStd := k32.NewProc("GetStdHandle")
	h, _, _ := getStd.Call(^uintptr(10)) // STD_OUTPUT_HANDLE = (DWORD)-11
	if h != 0 && h != ^uintptr(0) {
		return
	}
	attach := k32.NewProc("AttachConsole")
	// ATTACH_PARENT_PROCESS = (DWORD)-1
	if r, _, _ := attach.Call(^uintptr(0)); r == 0 {
		return
	}
	createFile := k32.NewProc("CreateFileW")
	open := func(name string) uintptr {
		p, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return ^uintptr(0)
		}
		h, _, _ := createFile.Call(
			uintptr(unsafe.Pointer(p)),
			0xC0000000, // GENERIC_READ|GENERIC_WRITE
			0x3,        // FILE_SHARE_READ|FILE_SHARE_WRITE
			0, 3, 0, 0) // OPEN_EXISTING
		return h
	}
	if h := open("CONOUT$"); h != ^uintptr(0) {
		f := os.NewFile(h, "CONOUT$")
		os.Stdout = f
		os.Stderr = f
		log.SetOutput(f) // 默认 logger 持有的是旧句柄,一并切换
	}
}

// msgboxFail GUI 子系统下控制台日志不可见, 启动失败用系统弹窗告知用户。
func msgboxFail(err error) {
	u32 := syscall.NewLazyDLL("user32.dll")
	mb := u32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("Cline Proxy")
	text, _ := syscall.UTF16PtrFromString("代理启动失败:\n" + err.Error() + "\n\n常见原因: 端口被占用, 可用 -port 换端口。")
	const mbIconError = 0x10
	mb.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbIconError)
	os.Exit(1)
}
