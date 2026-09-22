package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"cline-go-proxy/internal/kit"
)

// ============================================================================
// 管理后台访问控制
//
// 此前 /admin/api/* 全部零鉴权, 且:
//   - 默认监听 0.0.0.0(局域网任意设备可访问);
//   - 所有管理接口都套了 corsHandler, 无条件返回 Access-Control-Allow-Origin: *。
//
// 两者叠加出来的是一条完整可利用的攻击链: 用户只要在浏览器里打开任意网页,
// 该网页就能 fetch('http://127.0.0.1:3457/admin/api/accounts/export') 并把
// 响应体读走 —— 里面是全部 Cline 账号的 refreshToken(等于账号完全接管),
// 还有 /admin/api/keys 的代理密钥、/admin/api/zen/config 的上游 key。
// 防火墙拦不住这条路径, 因为请求确实发自用户本机。
//
// 现在的防线是两条, 缺一不可:
//   1. 管理接口必须携带访问令牌(首次启动生成, 落盘 data/admin-token, 0600)。
//      令牌通过 X-Admin-Token / Authorization: Bearer / admin_token Cookie 传递。
//   2. 管理接口不再返回 CORS 头。跨站页面即使猜到令牌也读不到响应体(同源策略)。
//      另加一道 Origin 预检: 带 Origin 且不是本机/私网来源的请求直接 403。
//
// 面板自身不受影响: 用带 ?token= 的地址打开一次, 服务端校验通过后种下
// HttpOnly Cookie, 之后同源的 fetch 会自动带上, 页面 JS 无需改动。
// ============================================================================

// adminTokenCookie 面板会话 Cookie 名。
const adminTokenCookie = "admin_token"

// adminCookieMaxAge 会话 Cookie 的有效期(180 天)。
//
// 之前没设 Max-Age, 是浏览器会话级 Cookie —— 关掉浏览器就失效, 用户重新打开
// 浏览器再访问 /admin/ 就会撞上"需要访问令牌"提示页, 体感上像"程序坏了"
// (2026-09-13 用户实测反馈)。令牌本身静态落在 data/admin-token, Cookie 值就是
// 令牌, 长效化不增加额外风险: 令牌一换, 旧 Cookie 校验自然失败; Cookie 自带
// HttpOnly + SameSite=Strict, 与令牌文件同一暴露面。
const adminCookieMaxAge = 180 * 24 * 3600

// adminTokenFile 令牌落盘位置。
func adminTokenFile() string { return kit.ResolveDataPath("admin-token") }

var (
	// adminTokenCache + adminTokenMu: 首次调用时启动横幅/托盘/adminStaticHandler/
	// adminAuth 可能同时进入, 两个 goroutine 都读到 "" 就各自 rand.Read 出不同
	// token, 但只写一个进 cache —— 结果横幅里打印的那个 token 用不了, 用户按提示
	// 打开面板反而 401。加一把锁串行化, 保证"生成一次、返回同一个值"。
	adminTokenCache string
	adminTokenMu    sync.Mutex
)

// loadOrCreateAdminToken 读取已有令牌, 不存在则生成并落盘。
// 进程内缓存, 避免每次请求都读磁盘。首次并发调用返回同一个值。
func loadOrCreateAdminToken() string {
	adminTokenMu.Lock()
	defer adminTokenMu.Unlock()
	if adminTokenCache != "" {
		return adminTokenCache
	}
	path := adminTokenFile()
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			adminTokenCache = t
			return t
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("  admin: 生成访问令牌失败, 管理接口将拒绝全部请求: %v", err)
		return ""
	}
	token := hex.EncodeToString(buf)
	if err := kit.WriteFileAtomicDefault(path, []byte(token+"\n")); err != nil {
		// 落盘失败不影响本次运行, 但重启后令牌会变, 必须让用户知道。
		log.Printf("  admin: 写入访问令牌失败(%v), 重启后令牌会变化", err)
	}
	adminTokenCache = token
	return token
}

// AdminToken 供启动横幅与托盘展示。
func AdminToken() string { return loadOrCreateAdminToken() }

// adminAuth 保护单个管理接口。
func adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Origin 预检必须排在令牌校验**之前**。
		//
		// 它挡的是"浏览器里的第三方页面"这一类场景, 与令牌是否正确无关:
		// 令牌一旦外泄(带 ?token= 的地址被分享、日志被翻), 任何网页都能拿它
		// 直接读取管理接口 —— 只有这一道检查能阻止浏览器把响应体交给页面。
		// 反过来(只在令牌失败后才查 Origin)等于这道防线形同虚设。
		if !originAllowed(r) {
			writeAPI(w, http.StatusForbidden, apiResponse{Error: "cross-origin admin request rejected"})
			return
		}
		token := loadOrCreateAdminToken()
		if token == "" {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "admin token unavailable"})
			return
		}
		provided := adminTokenFrom(r)
		if provided == "" || !tokenEqual(provided, token) {
			writeAPI(w, http.StatusUnauthorized, apiResponse{
				Error: "admin token required; 见启动横幅, 或 data/admin-token 文件, 或用带 ?token= 的地址打开面板",
			})
			return
		}
		// 令牌校验通过时顺手种 Cookie, 让面板后续同源请求自动带上。
		// Max-Age 必设: 会话级 Cookie 随浏览器关闭失效, 用户重开浏览器就会
		// 撞上"需要访问令牌"提示页(2026-09-13 实测反馈)。
		http.SetCookie(w, &http.Cookie{
			Name:     adminTokenCookie,
			Value:    token,
			Path:     "/",
			MaxAge:   adminCookieMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		next(w, r)
	}
}

// adminTokenFrom 从请求里取出待校验的令牌, 只支持头/Authorization/Cookie。
// 查询参数不在这里认(见函数末尾注释, P1-6)。
func adminTokenFrom(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Admin-Token")); v != "" {
		return v
	}
	if b := r.Header.Get("Authorization"); len(b) > 7 && strings.EqualFold(b[:7], "Bearer ") {
		return strings.TrimSpace(b[7:])
	}
	if c, err := r.Cookie(adminTokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	// ?token= 不在这里认: 它只在 /admin/ 页面的一次性 exchange 里生效
	// (见 adminStaticHandler: 校验通过 → 种 Cookie → 302 到不含 query 的地址),
	// API 一律走头/Authorization/Cookie, 避免令牌经 query 进入历史/Referer(P1-6)。
	return ""
}

// tokenEqual 定长比较, 避免按字节短路泄露令牌前缀。
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// originAllowed 判断浏览器跨站来源是否可信。
//
// 没有 Origin 头 = 非浏览器客户端(curl / SDK / 面板同源的简单 GET), 放行;
// 带 Origin 分两层判:
//  1. 同源: Origin 的 host[:port] 与本请求 Host 全等 —— 面板自身页面发出的
//     请求都走这条, 后面的端口校验不会把面板自己打挂。
//  2. 跨源: hostname 仍须是本机/私网(公网页面来这里拿数据是要挡的场景),
//     且**端口必须等于面板监听端口**(P1-5)。旧实现只看 hostname 完全
//     不比端口: 同机其它端口上的攻击页面与面板同 site, admin_token
//     Cookie(SameSite=Strict)会被浏览器一并带上, no-cors POST 就能静默
//     打穿 delete-all / refresh-all / generateKey 这类状态变更端点。
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// 同源请求(host+port 全等, 大小写不敏感): 面板页面自身的 fetch。
	if sameAuthority(u.Host, r.Host, u.Scheme) {
		return true
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil {
			// 形如 evil.com 的公网域名一律拒绝。
			return false
		}
		if !(ip.IsLoopback() || ip.IsPrivate()) {
			return false
		}
	}
	// 跨源放行还要求端口与面板一致。
	return authorityPort(u.Host, u.Scheme) == panelPort(r)
}

// sameAuthority 判断 Origin 的 authority 与请求 Host 是否同源(host+port 全等)。
// 任一侧省略端口按该 scheme 的默认端口(http 80 / https 443)补齐再比。
func sameAuthority(originHost, reqHost, scheme string) bool {
	oh, rh := strings.ToLower(strings.TrimSpace(originHost)), strings.ToLower(strings.TrimSpace(reqHost))
	if oh == rh {
		return true
	}
	ohn, op := splitAuthority(oh)
	rhn, rp := splitAuthority(rh)
	if ohn != rhn {
		return false
	}
	if op == "" {
		op = defaultPort(scheme)
	}
	if rp == "" {
		rp = defaultPort(scheme)
	}
	return op == rp
}

// splitAuthority 拆 host[:port]; 不含端口时 port 返回空串(不报错)。
// 处理 [::1]:8000 带方括号的 IPv6 形式。
func splitAuthority(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return strings.Trim(hostport, "[]"), ""
}

// defaultPort scheme 的默认端口; 不认识的 scheme 返回空串。
func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// authorityPort authority 的端口, 省略时按 scheme 补默认端口。
func authorityPort(hostport, scheme string) string {
	if _, p := splitAuthority(hostport); p != "" {
		return p
	}
	return defaultPort(scheme)
}

// panelPort 面板实际监听端口: 优先取本请求 Host 里的端口(浏览器发来的
// host:port), Host 没带端口(默认端口/测试桩)时退回全局监听地址。
func panelPort(r *http.Request) string {
	if _, p := splitAuthority(r.Host); p != "" {
		return p
	}
	if _, p := splitAuthority(proxyListenAddress); p != "" {
		return p
	}
	return ""
}

// wrapAdminTokenURL 把令牌拼进面板地址, 供启动横幅与托盘一键打开。
func wrapAdminTokenURL(base string) string {
	token := loadOrCreateAdminToken()
	if token == "" {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + url.QueryEscape(token)
}

// AdminPanelURL 返回带访问令牌的本机面板地址。
//
// 必须带令牌: 管理接口现在一律要求鉴权, 直接开 http://127.0.0.1:port/admin/
// 只会看到 401。服务端校验 ?token= 之后种 HttpOnly Cookie, 之后同源请求自动携带。
func AdminPanelURL(port int) string {
	return wrapAdminTokenURL(fmt.Sprintf("http://127.0.0.1:%d/admin/", port))
}
