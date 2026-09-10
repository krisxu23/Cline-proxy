//go:build windows

package app

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

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
	bmp = appendU16LE(bmp, 40)          // biSize
	bmp = appendU32LE(bmp, size)        // biWidth
	bmp = appendU32LE(bmp, size*2)      // biHeight (XOR+AND)
	bmp = appendU16LE(bmp, 1)           // biPlanes
	bmp = appendU16LE(bmp, 32)          // biBitCount
	bmp = appendU32LE(bmp, 0)           // biCompression
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
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止代理并退出程序")
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					OpenAdminWindow(adminURL)
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		fmt.Println("tray exited")
	})
}
