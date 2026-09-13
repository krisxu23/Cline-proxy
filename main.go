package main

import (
	"cline-go-proxy/internal/app"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func main() {
	loginMode := flag.Bool("login", false, "Run OAuth device login flow and add account to pool")
	// 默认只监听回环地址。
	//
	// 此前默认 0.0.0.0, 结果是同网段任意设备都能打开管理后台; 而管理后台
	// 明文提供全部账号 refreshToken 与上游 API Key。要局域网访问必须显式
	// 加 -host 0.0.0.0 (同时段网络自行承担风险, 管理接口仍需访问令牌)。
	host := flag.String("host", "127.0.0.1", "Listen host (127.0.0.1 = 仅本机; 0.0.0.0 = 允许局域网)")
	port := flag.Int("port", 3457, "Proxy server port")
	addAccount := flag.Bool("add-account", false, "Add a new account via OAuth to the pool")
	showList := flag.Bool("list", false, "List all accounts in the pool")
	startMode := flag.Bool("start", false, "Build, start proxy, and open admin panel in browser")
	desktopMode := flag.Bool("desktop", false, "Run as desktop app: tray icon + admin window (pairs with -ldflags \"-H=windowsgui\")")
	cliMode := flag.Bool("cli", false, "Windows: 留在前台控制台模式(调试用), 不进托盘")
	flag.Parse()

	// GUI 构建(windowsgui)下 CLI 模式需回挂父控制台才有输出
	if *loginMode || *addAccount || *showList || *startMode {
		attachParentConsole()
	}

	if *startMode {
		buildAndStart(*host, *port)
		return
	}

	if *loginMode || *addAccount {
		acc, err := app.AddAccountFromDeviceAuth()
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		fmt.Printf("Account added to pool successfully!\n")
		fmt.Printf("  Account ID: %s\n", acc.AccountID)
		fmt.Printf("  Email:      %s\n", acc.Email)
		fmt.Printf("  Status:     %s\n", acc.Status)
		fmt.Println("\nRun without flags to start the proxy with account rotation.")
		return
	}

	if *showList {
		accounts := app.ListAccounts()
		if len(accounts) == 0 {
			fmt.Println("No accounts in pool. Use --add-account to add one.")
			return
		}
		fmt.Printf("\n=== Account Pool (%d accounts) ===\n\n", len(accounts))
		for i, a := range accounts {
		fmt.Printf("  %d. [%s] %s (status: %s, used: %d, tokens: %d today / %d total)\n",
			i+1, a.AccountID, a.Email, a.Status, a.UsageCount, a.TokensToday, a.TokensTotal)
		}
		fmt.Println()
		return
	}

	// 桌面模式: Windows 上默认进入(交付的二进制是 windowsgui 子系统, 没有
	// 控制台 —— 走 CLI 分支等于双击之后什么都不发生, 连报错都看不见, 用户
	// 只会觉得程序没启动)。需要前台控制台调试时显式加 -cli。
	if *desktopMode || (runtime.GOOS == "windows" && !*cliMode) {
		runDesktop(*host, *port)
		return
	}

	if err := app.StartProxy(*host, *port); err != nil {
		log.Fatalf("Proxy failed: %v", err)
		os.Exit(1)
	}
}

// runDesktop 桌面模式主流程: 后台起服务 → 打开管理窗口 → 托盘阻塞。
func runDesktop(host string, port int) {
	adminURL := adminPanelURL(port)
	errCh := make(chan error, 1)
	go func() { errCh <- app.StartProxy(host, port) }()

	select {
	case err := <-errCh:
		// 服务起不来: 已有实例在跑就并入它的托盘+窗口, 否则弹窗报错
		if isServiceAlive(adminPanelBase(port)) {
			app.OpenAdminWindow(adminURL)
			app.RunTray(adminURL)
			return
		}
		msgboxFail(err)
	case <-time.After(1200 * time.Millisecond): // 等端口就绪再开窗口
	}
	go app.OpenAdminWindow(adminURL)
	app.RunTray(adminURL)
}

// adminPanelURL 带访问令牌的面板地址。管理接口现在一律要求鉴权,
// 不带令牌打开只会看到 401, 所以托盘与横幅统一开带令牌的地址。
func adminPanelURL(port int) string { return app.AdminPanelURL(port) }

// adminPanelBase 不带令牌的面板地址, 仅用于探测已有实例是否存活。
func adminPanelBase(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/admin/", port)
}

// isServiceAlive 探测管理入口是否已有实例在响应。
func isServiceAlive(adminURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(adminURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func buildAndStart(host string, port int) {
	exe := "cline-proxy.exe"
	isWindows := runtime.GOOS == "windows"
	if !isWindows {
		exe = "./cline-proxy"
	}

	fmt.Println("Building proxy...")
	// tags 不可省略: 缺少 with_utls/with_quic 时 reality、QUIC 类节点会被剔除
	args := []string{"build", "-tags", "with_quic,with_grpc,with_utls", "-ldflags", "-s -w"}
	if isWindows {
		// windowsgui 子系统: 双击启动不弹控制台窗口,托盘 + 管理窗口接管交互
		args = []string{"build", "-tags", "with_quic,with_grpc,with_utls", "-ldflags", "-s -w -H=windowsgui"}
	}
	args = append(args, "-o", exe, ".")
	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Build failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Build complete.")

	running := false
	if isWindows {
		out, _ := exec.Command("powershell", "-Command",
			"Get-Process cline-proxy -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id").Output()
		if len(out) > 0 {
			running = true
		}
	}

	if running {
		fmt.Println("Proxy is already running.")
	} else {
		fmt.Println("Starting proxy...")
		startArgs := []string{"-host", host, "-port", fmt.Sprintf("%d", port)}
		if isWindows {
			startArgs = append([]string{"-desktop"}, startArgs...)
		}
		startCmd := exec.Command(exe, startArgs...)
		startCmd.Stdout = os.Stdout
		startCmd.Stderr = os.Stderr
		if err := startCmd.Start(); err != nil {
			fmt.Printf("Start failed: %v\n", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
		fmt.Println("Proxy started.")
	}

	url := adminPanelURL(port)
	fmt.Printf("\nAdmin panel: %s\n", url)

	switch runtime.GOOS {
	case "windows":
		app.OpenAdminWindow(url)
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
