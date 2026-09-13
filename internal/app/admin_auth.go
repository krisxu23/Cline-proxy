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

// adminTokenFile 令牌落盘位置。
func adminTokenFile() string { return kit.ResolveDataPath("admin-token") }

var adminTokenCache string

// loadOrCreateAdminToken 读取已有令牌, 不存在则生成并落盘。
// 进程内缓存, 避免每次请求都读磁盘。
func loadOrCreateAdminToken() string {
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
		http.SetCookie(w, &http.Cookie{
			Name:     adminTokenCookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		next(w, r)
	}
}

// adminTokenFrom 从请求里取出待校验的令牌, 支持头/Authorization/Cookie/查询参数。
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
	// ?token= 只用于"首次打开面板"引导, 校验通过后即改走 Cookie。
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// tokenEqual 定长比较, 避免按字节短路泄露令牌前缀。
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// originAllowed 判断浏览器跨站来源是否可信。
//
// 没有 Origin 头 = 非浏览器客户端(curl / SDK / 面板同源的简单 GET), 放行;
// 带 Origin 就必须是本机或私网 —— 公网页面来这里拿数据正是要挡的场景。
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 形如 evil.com 的公网域名一律拒绝。
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
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
