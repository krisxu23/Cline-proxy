package app

import (
	"bytes"
	"fmt"
	"free-router/internal/kit"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

// freePort 启动前清理占着目标端口的「旧实例」。
//
// 此前这里是无条件的 Stop-Process: 只要端口被占, 就把占用者的进程强杀, 既没有
// 任何身份校验也没有提示 —— 用户机器上恰好用该端口的无关程序会被静默干掉。
// 现在只清理与当前可执行文件同名的进程(即另一个 free-router), 遇到陌生进程
// 如实记录后放手, 让 ListenAndServe 用 "address already in use" 明确报错。
func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if !portInUse(addr) {
		return // port is free
	} else if runtime.GOOS != "windows" {
		// 非 Windows 没有可靠的「按端口找进程」手段。旧实现在这里会去执行
		// 根本不存在的 powershell, 失败后仍进入 5 秒重试循环, 而每次都能
		// 拨通旧进程 —— 等于白等 5 秒才失败。直接交给监听报错更清楚。
		return
	}
	if !killOwnProcessOnPort(port) {
		return
	}
	// 杀进程后确认端口确实释放，避免旧进程尚未退出时立刻竞争监听。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !portInUse(addr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("  端口 %d 仍被占用, 监听可能失败", port)
}

// portInUse 目标地址当前是否有进程在监听。
func portInUse(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// selfProcessName 当前可执行文件名(去扩展名), 用于识别「自己人」。
func selfProcessName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
}

// killOwnProcessOnPort 只终止与自身同名的进程占用的端口, 返回是否杀掉过进程。
//
// 命令注入面说明(2026-09-16 审计 P3-12): 两处 PowerShell 调用都是参数分离的
// ExecCommand(不经 shell), 且脚本里唯一的插值点是 `%d` 端口号 —— 它是 int,
// 格式化后必然只含数字, 不可能携带引号或分号; 进程名与 pid 则来自
// Get-NetTCPConnection / Get-Process 的**系统枚举结果**, 不是用户输入, 且进程名
// 只用于 EqualFold 比较(不用来拼脚本), 真正下杀手的只有 pid, 也经 %s 传入但来源
// 同上。因此当前不存在注入路径。
//
// 为什么用 PowerShell 而不是纯 Go: Windows 上"端口 → 占用进程"需要
// GetExtendedTcpTable / netstat 解析, Go 标准库没有等价 API; 走 CIM cmdlet 是
// 最短且可读的路径。若将来要彻底去掉外部进程依赖, 可换成 iphlpapi 的 syscall 封装。
func killOwnProcessOnPort(port int) bool {
	self := selfProcessName()
	if self == "" {
		return false
	}
	// 只列 Listen 态连接, 并带上进程名, 供下面按名字过滤。
	script := fmt.Sprintf(
		`$p=Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue; `+
			`if($p){ $p.OwningProcess | Sort-Object -Unique | ForEach-Object { `+
			`$proc=Get-Process -Id $_ -ErrorAction SilentlyContinue; `+
			`if($proc){ Write-Output "$($proc.Id) $($proc.ProcessName)" } } }`, port)
	out, err := kit.ExecCommand("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		log.Printf("  端口 %d 被占用, 但无法枚举占用进程: %v", port, err)
		return false
	}

	selfPID := strconv.Itoa(os.Getpid())
	killed := false
	var foreign []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, name := fields[0], fields[1]
		if !strings.EqualFold(name, self) {
			foreign = append(foreign, name+"(pid "+pid+")")
			continue
		}
		if pid == selfPID {
			continue // 绝不自杀
		}
		stop := kit.ExecCommand("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("Stop-Process -Id %s -Force -ErrorAction SilentlyContinue", pid))
		if err := stop.Run(); err == nil {
			killed = true
			log.Printf("  已终止占用端口 %d 的旧实例 %s(pid %s)", port, name, pid)
		}
	}
	if len(foreign) > 0 {
		log.Printf("  端口 %d 被无关进程占用, 不会强杀: %s", port, strings.Join(foreign, ", "))
	}
	return killed
}

// parseInferenceCapDuration 从 Cline 429 错误体中解析 "Try again in 17h 59m" 形式的等待时长。
// 支持 "17h 59m"、"17h"、"59m"、"30s"、"1d 2h 30m" 等组合。
func parseInferenceCapDuration(body string) time.Duration {
	// 在错误体中查找 "Try again in ..." 子串
	idx := strings.Index(body, "Try again in")
	if idx < 0 {
		return 0
	}
	rest := body[idx+len("Try again in"):]
	// 截取到下一个引号或换行
	end := len(rest)
	if i := strings.IndexAny(rest, "\"\n\r}"); i >= 0 {
		end = i
	}
	segment := strings.TrimSpace(rest[:end])
	return parseHumanDuration(segment)
}

// parseHumanDuration 解析 "17h 59m" / "2h" / "59m" / "30s" / "1d 2h" 之类的时长。
func parseHumanDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	var total time.Duration
	num := 0
	valid := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			valid = true
		case c == 'd':
			total += time.Duration(num) * 24 * time.Hour
			num, valid = 0, false
		case c == 'h':
			total += time.Duration(num) * time.Hour
			num, valid = 0, false
		case c == 'm' && i+1 < len(s) && s[i+1] == 's':
			total += time.Duration(num) * time.Millisecond
			num, valid = 0, false
			i++
		case c == 'm':
			total += time.Duration(num) * time.Minute
			num, valid = 0, false
		case c == 's':
			total += time.Duration(num) * time.Second
			num, valid = 0, false
		case c == ' ':
			// 分隔符
		default:
			// 未知字符，重置
			num, valid = 0, false
		}
	}
	if total <= 0 {
		return 0
	}
	_ = valid
	return total
}

// parseRetryAfter 解析 HTTP Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	// 尝试秒数
	if secs, err := parseIntSafe(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	// 尝试 HTTP 日期
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func parseIntSafe(s string) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
