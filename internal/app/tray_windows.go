//go:build windows

package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"cline-go-proxy/internal/kit"
	"fyne.io/systray"
)

// trayIcon 生成托盘图标: 32x32 蓝色圆角方块 + 白色 "C" 圆环, 纯代码生成避免资产文件。
func trayIcon() []byte {
	const size = 32
	const radius = 7.0   // 圆角半径
	const ringR = 8.5    // "C" 圆环半径
	const ringW = 3.2    // 圆环宽度
	cx, cy := 16.0, 16.0 // 圆心

	px := make([]byte, size*size*4) // BGRA, 自底向上
	setPixel := func(x, y int, r, g, b, a byte) {
		// ICO XOR 数据自底向上存储
		row := size - 1 - y
		off := (row*size + x) * 4
		px[off], px[off+1], px[off+2], px[off+3] = b, g, r, a
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			// 圆角方块判定
			nx, ny := math.Max(radius-fx, 0), math.Max(radius-fy, 0)
			insideRect := fx >= radius && fx <= size-radius || fy >= radius && fy <= size-radius
			cornerOK := nx*nx+ny*ny <= radius*radius
			if !(insideRect || cornerOK) {
				setPixel(x, y, 0, 0, 0, 0)
				continue
			}
			// "C" 圆环: 命中环带且缺口朝右(±50°)
			dx, dy := fx-cx, fy-cy
			d := math.Sqrt(dx*dx + dy*dy)
			ang := math.Atan2(dy, dx) * 180 / math.Pi
			inRing := math.Abs(d-ringR) <= ringW/2 && (ang < -50 || ang > 50)
			if inRing {
				setPixel(x, y, 255, 255, 255, 255)
			} else {
				setPixel(x, y, 235, 99, 37, 255) // #2563EB 存 BGRA: B=235 G=99 R=37
			}
		}
	}

	// 打包 ICO: ICONDIR + ICONDIRENTRY + BITMAPINFOHEADER + XOR + AND 掩码
	andMask := make([]byte, size/8*size) // 4 字节/行 × 32 行, alpha 已带透明度, 掩码全 0
	bmp := make([]byte, 0, 40+len(px)+len(andMask))
	bmp = appendU16LE(bmp, 40)     // biSize
	bmp = appendU32LE(bmp, size)   // biWidth
	bmp = appendU32LE(bmp, size*2) // biHeight (XOR+AND)
	bmp = appendU16LE(bmp, 1)      // biPlanes
	bmp = appendU16LE(bmp, 32)     // biBitCount
	bmp = appendU32LE(bmp, 0)      // biCompression
	bmp = appendU32LE(bmp, uint32(len(px)+len(andMask)))
	bmp = appendU32LE(bmp, 0, 0, 0, 0) // biXPels, biYPels, biClrUsed, biClrImportant
	bmp = append(bmp, px...)
	bmp = append(bmp, andMask...)

	ico := make([]byte, 0, 22+len(bmp))
	ico = appendU16LE(ico, 0, 1, 1) // reserved, type=icon, count=1
	ico = append(ico, size, size, 0, 0)
	ico = appendU16LE(ico, 1, 32)
	ico = appendU32LE(bmp)
	_ = bmp
	ico = appendU32LE(ico, uint32(len(bmp)))
	ico = appendU32LE(ico, 22) // 数据偏移
	ico = append(ico, bmp...)
	return ico
}

func appendU16LE(dst []byte, vals ...uint16) []byte {
	for _, v := range vals {
		dst = append(dst, byte(v), byte(v>>8))
	}
	return dst
}

func appendU32LE(dst []byte, vals ...uint32) []byte {
	for _, v := range vals {
		dst = append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return dst
}

// appBrowsers 支持应用窗口模式(--app)的 Chromium 系浏览器, 按优先级排列。
var appBrowsers = []string{
	`Microsoft\Edge\Application\msedge.exe`,
	`Google\Chrome\Application\chrome.exe`,
}

// findAppBrowser 在常见安装目录查找支持 --app 模式的浏览器; 找不到返回空串。
func findAppBrowser() string {
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LocalAppData"),
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, rel := range appBrowsers {
			p := filepath.Join(root, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// OpenAdminWindow 用 Edge/Chrome 应用模式打开管理界面(独立窗口、无地址栏、
// 独立任务栏图标,观感即桌面应用); 都没有时回退到默认浏览器。
func OpenAdminWindow(adminURL string) {
	if browser := findAppBrowser(); browser != "" {
		// ponytail: --app 窗口由用户手动关闭; 服务端继续运行,托盘可再次打开。
		cmd := exec.Command(browser, "--app="+adminURL, "--window-size=1280,860")
		if err := cmd.Start(); err == nil {
			return
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", adminURL).Start()
}

// RunTray 启动系统托盘并阻塞,直到用户点击「退出」。
func RunTray(adminURL string) {
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTitle("Cline Proxy")
		systray.SetTooltip("Cline Go Proxy 综合网关 - 运行中")
		mOpen := systray.AddMenuItem("打开管理界面", "在应用窗口中打开 Web 后台")
		mData := systray.AddMenuItem("打开数据目录", "在资源管理器中打开 data/ 目录(日志与配置所在)")
		mDiag := systray.AddMenuItem("导出诊断包", "打包 health/日志尾部/节点概览为 zip, 便于排障")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止代理并退出程序")
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					OpenAdminWindow(adminURL)
				case <-mData.ClickedCh:
					openDataDir()
				case <-mDiag.ClickedCh:
					exportDiagnostics(adminURL)
				case <-mQuit.ClickedCh:
					// 优雅退出: 停服 → 刷请求日志 → 关节点盒 → 退出进程。
					GracefulExit()
					return
				}
			}
		}()
	}, func() {
		fmt.Println("tray exited")
	})
}

// openDataDir 在资源管理器中打开 data/ 目录(日志、配置、令牌都在这里)。
func openDataDir() {
	dir := filepath.Dir(kit.ResolveDataPath("cline-proxy.log"))
	exec.Command("explorer", dir).Start()
}

// exportDiagnostics 把当前运行状态的快照打包成 data/diag-<时间戳>.zip, 便于排障:
//   - health.json: 实时 /health(含 nodePool/subNodes/logBytes/dropped 等);
//   - logs/*: 主日志与流式日志的尾部(不复制整份, 控制体积);
//   - nodes.json: 监听地址、出口节点数、订阅节点数;
//   - data_listing.txt: data/ 目录清单(只列文件名与大小, 不复制内容, 避免把
//     admin-token 等敏感文件带出去);
//   - version.txt: 构建版本与 Go 运行时版本。
//
// 默认导出到 data/diag-<时间戳>.zip, 完成后用资源管理器定位到该文件方便取走。
func exportDiagnostics(adminURL string) {
	dir := filepath.Dir(kit.ResolveDataPath("cline-proxy.log"))
	ts := time.Now().Format("20060102-150405")
	zipPath := filepath.Join(dir, "diag-"+ts+".zip")
	if err := writeDiagnosticsZip(zipPath, adminURL); err != nil {
		log.Printf("diag: 导出失败: %v", err)
		return
	}
	// 用资源管理器定位到该文件, 用户无需自己翻目录。
	exec.Command("explorer", "/select,", zipPath).Start()
}

func writeDiagnosticsZip(zipPath, adminURL string) error {
	dir := filepath.Dir(kit.ResolveDataPath("cline-proxy.log"))
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	var werr error
	writeZipFile := func(name string, data []byte) {
		if werr != nil {
			return
		}
		w, e := zw.Create(name)
		if e != nil {
			werr = e
			return
		}
		if _, e := w.Write(data); e != nil {
			werr = e
		}
	}

	// 1) /health 实时快照(真实端点)。本机探测走无代理 client, 避免被环境的
	//    HTTP_PROXY 拦掉。
	if u, e := url.Parse(adminURL); e == nil {
		healthURL := "http://" + u.Host + "/health"
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
		if resp, e2 := client.Get(healthURL); e2 == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			writeZipFile("health.json", b)
		} else {
			writeZipFile("health.json", []byte("error: "+e2.Error()))
		}
	}

	// 2) 主日志与流式日志的尾部(各 64KB, 控制体积)。
	writeZipFile("logs/cline-proxy.log.tail", readTailFile(kit.ResolveDataPath("cline-proxy.log"), 64<<10))
	writeZipFile("logs/cline-proxy-stream.log.tail", readTailFile(kit.ResolveDataPath("cline-proxy-stream.log"), 64<<10))

	// 3) 节点与订阅概览。nodePorts 由 nodeMu 保护, 短暂加锁读长度。
	nodeMu.Lock()
	np := len(nodePorts)
	nodeMu.Unlock()
	nodesJSON, _ := json.MarshalIndent(map[string]any{
		"listen":   proxyListenAddress,
		"nodePool": np,
		"subNodes": len(subNodeKeysSnapshot()),
	}, "", "  ")
	writeZipFile("nodes.json", nodesJSON)

	// 4) data/ 目录清单(只列名字与大小, 不复制内容, 避免敏感文件外泄)。
	writeZipFile("data_listing.txt", dataDirListing(dir))

	// 5) 版本信息。
	ver := "buildVersion=" + buildVersion + "\nGo=" + runtime.Version() + "\n"
	writeZipFile("version.txt", []byte(ver))

	return werr
}

// readTailFile 读取文件尾部至多 n 字节; 文件不存在/为空时返回说明文本。
func readTailFile(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return []byte("error: " + err.Error())
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return []byte("error: " + err.Error())
	}
	size := st.Size()
	if size == 0 {
		return []byte("(empty)")
	}
	if size <= n {
		b, _ := io.ReadAll(f)
		return b
	}
	if _, err = f.Seek(size-n, io.SeekStart); err != nil {
		return []byte("error: " + err.Error())
	}
	buf := make([]byte, n)
	m, _ := f.Read(buf)
	return buf[:m]
}

// dataDirListing 列出 data/ 下的文件名、大小与修改时间(不含内容)。
func dataDirListing(dir string) []byte {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []byte("error: " + err.Error())
	}
	var b bytes.Buffer
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%-40s %12d  %s\n", e.Name(), info.Size(), info.ModTime().Format("2006-01-02 15:04:05"))
	}
	return b.Bytes()
}
