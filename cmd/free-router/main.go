package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"free-router/internal/app"
	"io"
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
	}
}

// runDesktop 桌面模式主流程: 后台起服务 → 首次运行开管理窗口 → 托盘阻塞。
func runDesktop(host string, port int) {
	adminURL := adminPanelURL(port)
	// errCh 只会收到一次(StartProxy 只返回一次), 必须保证只被消费一次:
	// 之前两段 select 各 <-errCh, 第一段取走后第二段的 goroutine 会永久阻塞,
	// 靠进程退出掩盖(P3-13)。consumed 标记保证二次监听只在未被取走时挂上。
	errCh := make(chan error, 1)
	go func() { errCh <- app.StartProxy(host, port) }()

	consumed := false
	select {
	case err := <-errCh:
		// 快速失败: 服务起不来且没有其他实例在跑, 直接弹窗报错;
		// 否则(成功, 或端口被已有实例占用)继续往下走正常进入流程。
		consumed = true
		if err != nil && !isServiceAlive(adminHealthBase(port)) {
			msgboxFail(err)
			return
		}
	case <-time.After(300 * time.Millisecond):
	}
	// 常驻兜底: StartProxy 可能在更晚才失败(例如 sing-box 节点初始化要数秒),
	// 那时窗口已开、errCh 也没人读, 错误会被静默吞掉——而 GUI 无控制台,
	// 用户什么都看不到。这里一直监听, 收到非 nil 错误且当前没有其他实例在跑,
	// 就弹窗告知。成功(nil)或端口被已有实例占用则忽略。
	// 仅在第一段没消费掉 errCh 时才挂: 否则这个 <-errCh 永远等不到第二次发送。
	if !consumed {
		go func() {
			if err := <-errCh; err != nil && !isServiceAlive(adminHealthBase(port)) {
				msgboxFail(err)
			}
		}()
	}
	// 每次启动都自动弹管理窗口(2026-09-24 用户要求: 打开就能看到后台在哪)。
	//
	// ★ 必须在**服务真正就绪之后**才开窗, 且不能阻塞托盘。两件事都有实测依据:
	//
	//  1. 就绪时机: StartProxy 在开始监听**之前**要做同步初始化(loadPool +
	//     账号 token 预热 + initRegionModels + syncNodeBox 构建 sing-box 节点池),
	//     实测从进程启动到 3457 开始监听要 **16 秒**(2026-09-24 日志: 06:34:08
	//     进程启动 → 06:34:24 admin panel 就绪)。此前只等固定 1.2 秒就开窗,
	//     Chrome 打开的是一个还没监听的服务 —— 用户看到浏览器错误页, 关掉过一会儿
	//     重开才正常。所以改为轮询 /health 直到真正可用(见 waitAdminReady)。
	//  2. 顺序: 先起托盘。托盘图标立刻出现 = 用户马上看到"程序起来了", 否则双击后
	//     十几秒毫无反馈; 面板在就绪后由后台协程弹出。
	//
	// 失败必须落日志: 交付的二进制是 windowsgui 子系统、没有控制台, 静默失败
	// 时用户只能看到"双击了没反应"(见 OpenAdminWindow 的错误返回)。
	// 注意日志里**不能**带 adminURL —— 它含访问令牌。
	go func() {
		if !waitAdminReady(adminHealthBase(port), adminReadyWait) {
			log.Printf("  admin panel: 等待服务就绪超时(%v), 仍打开管理界面(可能显示连接错误; 可稍后从托盘「打开管理界面」重开)", adminReadyWait)
		}
		if err := app.OpenAdminWindow(adminURL); err != nil {
			log.Printf("  admin panel: 自动打开管理界面失败: %v (可从托盘「打开管理界面」重试, 或手动访问 http://127.0.0.1:%d/admin/)", err, port)
		}
	}()
	app.RunTray(adminURL)
}

// adminPanelURL 带访问令牌的面板地址。管理接口现在一律要求鉴权,
// 不带令牌打开只会看到 401, 所以托盘与横幅统一开带令牌的地址。
func adminPanelURL(port int) string { return app.AdminPanelURL(port) }

// adminHealthBase 健康端点地址, 仅用于探测已有实例是否真正可用。
func adminHealthBase(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/health", port)
}

// adminReadyWait 开管理窗口前等待服务就绪的上限。
//
// 为什么不能等固定时长: StartProxy 在开始监听**之前**要做同步初始化(loadPool +
// 账号 token 预热 + initRegionModels + syncNodeBox 构建 sing-box 节点池), 实测
// 从进程启动到 3457 开始监听要 **16 秒**(2026-09-24 日志: 06:34:08 进程启动 →
// 06:34:24 admin panel 就绪)。此前只等 1.2 秒就开窗, Chrome 打开的是一个还没
// 监听的服务 —— 用户看到浏览器错误页, 关掉过一会儿重开才正常(用户实证)。
//
// 给 30 秒余量: 订阅节点多、机器慢时会更久。超时**不阻止**开窗 —— 让用户看到
// "连不上"也比什么都不弹更可诊断, 且托盘随时可再开。
const adminReadyWait = 30 * time.Second

// adminReadyPollGap 就绪轮询间隔。
//
// isServiceAlive 自身有 2s 的 HTTP 超时, 未就绪时每次探测都要等满它, 所以实际
// 节奏 ≈ 2s 一次; 这个间隔只是避免服务已就绪却被反复忙等。
const adminReadyPollGap = 250 * time.Millisecond

// waitAdminReady 轮询健康端点直到服务可用或超时; 返回 false 表示超时。
//
// 复用 isServiceAlive(打 /health 并校验响应 JSON 里的 version 字段)而不是
// 只判"端口可连": 端口刚 bind、路由还没挂上的窗口里, 只判可连会得到假阳性,
// 开出来的窗口照样是错误页。
func waitAdminReady(healthURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if isServiceAlive(healthURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(adminReadyPollGap)
	}
}

// isServiceAlive 探测已有实例是否真的在正常服务。
//
// 之前打 /admin/ 只判 200: 未登录的令牌提示页恒 200(admin.go), 半启动 /
// 配置错误的实例同样会被判成"活着", 探活形同虚设(P3-14)。改为打 /health
// 并校验响应 JSON 里带预期标识 version 字段(见 proxy.go healthInfo)。
func isServiceAlive(healthURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return false
	}
	_, ok := payload["version"]
	return ok
}

func buildAndStart(host string, port int) {
	exe := "free-router.exe"
	isWindows := runtime.GOOS == "windows"
	if !isWindows {
		exe = "./free-router"
	}

	fmt.Println("Building proxy...")
	// tags 不可省略: 缺少 with_utls/with_quic 时 reality、QUIC 类节点会被剔除
	args := []string{"build", "-tags", "with_quic,with_grpc,with_utls", "-ldflags", "-s -w"}
	if isWindows {
		// windowsgui 子系统: 双击启动不弹控制台窗口,托盘 + 管理窗口接管交互
		args = []string{"build", "-tags", "with_quic,with_grpc,with_utls", "-ldflags", "-s -w -H=windowsgui"}
	}
	// 入口包已迁到 cmd/free-router(标准布局), 构建目标随之。
	args = append(args, "-o", exe, "./cmd/free-router")
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
			"Get-Process free-router -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id").Output()
		if len(out) > 0 {
			running = true
		}
	} else if isServiceAlive(adminHealthBase(port)) {
		// 非 Windows 此前恒 false: 无探活必然重复起第二实例(端口被占,
		// 第二实例 ListenAndServe 失败但面板照开)。复用 /health 探活防重复。
		running = true
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

	// 统一走 app.OpenAdminWindow: 它内部已按平台分流(Windows 用 Edge/Chrome --app,
	// 其它平台 open/xdg-open), 这里的 switch 是重复的; 且它的错误返回需要被记录。
	if err := app.OpenAdminWindow(url); err != nil {
		log.Printf("admin panel: 打开浏览器失败: %v", err)
	}
}
