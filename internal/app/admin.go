package app

import (
	"cline-go-proxy/internal/webui"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", adminStaticHandler)
	mux.HandleFunc("/admin/api/accounts", adminAuth(handleAdminAccounts))
	mux.HandleFunc("/admin/api/accounts/add", adminAuth(handleAdminAccountAdd))
	mux.HandleFunc("/admin/api/accounts/delete", adminAuth(handleAdminAccountDelete))
	mux.HandleFunc("/admin/api/accounts/test", adminAuth(handleAdminAccountTest))
	mux.HandleFunc("/admin/api/oauth/start", adminAuth(handleOAuthStart))
	mux.HandleFunc("/admin/api/oauth/status", adminAuth(handleOAuthStatus))
	mux.HandleFunc("/admin/api/sso/import", adminAuth(handleSSOImport))
	mux.HandleFunc("/admin/api/stats", adminAuth(handleAdminStats))
	mux.HandleFunc("/admin/api/batch-import", adminAuth(handleBatchImport))
	mux.HandleFunc("/admin/api/accounts/refresh-all", adminAuth(handleAdminRefreshAll))
	mux.HandleFunc("/admin/api/accounts/delete-all", adminAuth(handleAdminDeleteAll))
	mux.HandleFunc("/admin/api/accounts/reset", adminAuth(handleAdminAccountReset))
	mux.HandleFunc("/admin/api/accounts/export", adminAuth(handleAccountsExport))
	mux.HandleFunc("/admin/api/logs", adminAuth(handleRequestLogs))
	mux.HandleFunc("/admin/api/logs/trace", adminAuth(handleLogTrace))
	mux.HandleFunc("/admin/api/config/export", adminAuth(handleConfigExport))
	mux.HandleFunc("/admin/api/health", adminAuth(handleHealth))
	mux.HandleFunc("/admin/api/nodes/blacklist", adminAuth(handleNodeBlacklist))
	mux.HandleFunc("/admin/api/nodes/blacklist/clear", adminAuth(handleNodeBlacklistClear))
	mux.HandleFunc("/admin/api/nodes/blacklist/list", adminAuth(handleNodeBlacklistList))
	mux.HandleFunc("/admin/api/nodes/traffic/history", adminAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
			return
		}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"history": nodeTrafficHistory()}})
	}))
	mux.HandleFunc("/admin/api/nodes/traffic/reset", adminAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
			return
		}
		nodeTrafficReset()
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"reset": true}})
	}))
	mux.HandleFunc("/admin/api/config/import", adminAuth(handleConfigImport))
	mux.HandleFunc("/admin/api/keys", adminAuth(handleAdminGetKeys))
	mux.HandleFunc("/admin/api/keys/generate", adminAuth(handleAdminGenerateKey))
	mux.HandleFunc("/admin/api/keys/delete", adminAuth(handleAdminDeleteKey))
	mux.HandleFunc("/admin/api/models", adminAuth(handleAdminModels))
	mux.HandleFunc("/admin/api/models/refresh", adminAuth(handleAdminModelsRefresh))
	mux.HandleFunc("/admin/api/config", adminAuth(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", adminAuth(handleAdminUpdateConfig))
	mux.HandleFunc("/admin/api/config/headers/sync", adminAuth(handleAdminHeadersSync))
	mux.HandleFunc("/admin/api/opencode/config", adminAuth(handleZenConfig))
	mux.HandleFunc("/admin/api/opencode/config/update", adminAuth(handleZenConfigUpdate))
	mux.HandleFunc("/admin/api/opencode/nodes", adminAuth(handleZenNodes))
	mux.HandleFunc("/admin/api/opencode/nodes/check", adminAuth(handleZenNodesCheck))
	mux.HandleFunc("/admin/api/opencode/models", adminAuth(handleZenModels))
	mux.HandleFunc("/admin/api/opencode/models/refresh", adminAuth(handleZenModelsRefresh))
	mux.HandleFunc("/admin/api/opencode/stats", adminAuth(handleZenStats))
	// 旧 zen 路径别名,兼容旧引用
	mux.HandleFunc("/admin/api/zen/config", adminAuth(handleZenConfig))
	mux.HandleFunc("/admin/api/zen/config/update", adminAuth(handleZenConfigUpdate))
	mux.HandleFunc("/admin/api/zen/models", adminAuth(handleZenModels))
	mux.HandleFunc("/admin/api/zen/models/refresh", adminAuth(handleZenModelsRefresh))
	mux.HandleFunc("/admin/api/zen/stats", adminAuth(handleZenStats))
	mux.HandleFunc("/admin/api/providers", adminAuth(handleProvidersConfig))
	mux.HandleFunc("/admin/api/providers/update", adminAuth(handleProvidersUpdate))
	mux.HandleFunc("/admin/api/providers/refresh", adminAuth(handleProvidersRefresh))
	mux.HandleFunc("/admin/api/providers/test", adminAuth(handleProvidersTest))
	mux.HandleFunc("/admin/api/router", adminAuth(handleAdminRouter))
	mux.HandleFunc("/admin/api/router/save", adminAuth(handleAdminRouterSave))
	mux.HandleFunc("/admin/api/router/preview", adminAuth(handleRoutePreview))
	mux.HandleFunc("/admin/api/router/validate", adminAuth(handleAdminRouterValidate))
	mux.HandleFunc("/admin/api/router/refresh", adminAuth(handleAdminRouterRefresh))
	mux.HandleFunc("/admin/api/router/maintenance", adminAuth(handleAdminRouterMaintenance))
	// ClinePass 订阅池管理
	registerClinePassAdminRoutes(mux)
	mux.HandleFunc("/admin/zen/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
}

// renderAdminTokenPrompt 无令牌访问 /admin/ 时返回的引导页。
//
// 此前 /admin/ 对任何本地进程都返回完整 121KB 外壳(含全部前端 JS), 同机多账号
// 共享场景等于把管理界面裸暴露。现在只有带有效令牌才回完整外壳, 否则回这段几百
// 字节、不含任何前端业务脚本的引导页。注意它是 200 而非 401: 用户双击 exe 后
// 浏览器第一次访问必然没 token, 得让他「看得到去哪拿 token」。
//
// 2026-09-13 用户实测反馈"程序打不开了, 打开显示这个"后, 在提示页上补了一个
// 令牌输入框: 用户从 data/admin-token 复制内容粘进来即可进入, 不必再手工拼
// ?token= 地址。同时把会话 Cookie 改成 180 天长效(见 adminCookieMaxAge),
// 修掉"关一次浏览器就要求重新带令牌"的体验问题。现在还会区分"没带令牌"与"令牌无效":
// mismatch=true 时提示页显示"访问令牌无效，请重新输入"并回填输入内容。
//
// 现在区分两种失败: 完全没带令牌(mismatch=false, 干净引导页) vs 带了令牌但校验没过
// (mismatch=true, 明确提示"访问令牌无效，请重新输入"并把用户刚输入的内容回填到输入框,
// 避免让他重新手抄一遍)。retain 是用户上一次输入(通常来自 URL ?token= 或旧 Cookie 值),
// 回填前必须过一遍 HTML 转义, 否则令牌里的 " < > & 会破坏属性、造成注入。
func renderAdminTokenPrompt(mismatch bool, retain string) string {
	var errBanner, val string
	if mismatch {
		errBanner = `<p style="margin:0 0 14px;padding:10px 12px;border-radius:8px;color:#fca5a5;background:rgba(220,38,38,.12);border:1px solid rgba(220,38,38,.45)"><b>访问令牌无效，请重新输入。</b></p>`
		val = template.HTMLEscapeString(retain)
	}
	return `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Cline Proxy · 需要访问令牌</title>
<style>body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}main{max-width:560px;padding:32px;background:#1e293b;border-radius:12px;line-height:1.7}h1{font-size:20px;margin:0 0 12px}code{background:#0f172a;padding:2px 6px;border-radius:4px;word-break:break-all}a{color:#60a5fa}p{margin:10px 0}form{display:flex;gap:8px;margin:14px 0 4px}input{flex:1;background:#0f172a;border:1px solid #334155;border-radius:6px;color:#e2e8f0;padding:8px 10px;font-size:14px}button{background:#2563eb;border:0;border-radius:6px;color:#fff;padding:8px 16px;font-size:14px;cursor:pointer}button:hover{background:#1d4ed8}</style>
</head>
<body><main>
<h1>管理后台需要访问令牌</h1>
<p>本页面未携带有效的访问令牌, 因此只返回这段引导, 不加载完整管理界面(含全部前端脚本)。</p>
<p><b>程序在正常运行</b> —— 这不是启动失败。双击 exe 自动弹出的窗口、以及托盘「打开管理界面」打开的地址, 都已自带令牌, 用那些入口打开不会看到本页。</p>
` + errBanner + `
<form onsubmit="var v=document.getElementById('tk').value.replace(/\s+/g,'');if(v){location.href='/admin/?token='+encodeURIComponent(v);}return false;">
<input id="tk" type="password" autocomplete="off" placeholder="粘贴 data/admin-token 文件内容" value="` + val + `">
<button type="submit">进入管理界面</button>
</form>
<p>令牌位置(任选其一):</p>
<p>1. 数据目录下的 <code>data/admin-token</code> 文件, 复制内容粘贴到上面输入框。</p>
<p>2. 托盘菜单「打开数据目录」可直接定位到该文件。</p>
<p>令牌校验通过后会种下 <b>180 天有效</b> 的会话 Cookie(HttpOnly), 之后直接打开 <code>/admin/</code> 无需再带 token; 更换令牌后旧 Cookie 自动失效。</p>
</main></body></html>`
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		// 校验令牌: 支持 ?token= 或已种下的会话 Cookie。面板 HTML 本身不含任何
		// 凭据, 但此前对**任何**本地进程都返回完整 121KB 外壳(含全部前端 JS),
		// 同机多账号共享场景下等于把管理界面裸暴露。现在改为: 没令牌只回一个
		// 几百字节的「请附带令牌」提示页, 带令牌才回完整外壳。
		ok := false
		var provided string
		var mismatch bool
		if token := loadOrCreateAdminToken(); token != "" {
			if provided = adminTokenFrom(r); provided != "" {
				if tokenEqual(provided, token) {
					ok = true
					// 令牌来自查询参数时种下会话 Cookie, 后续同源请求自动携带。
					// Max-Age 必设(见 adminCookieMaxAge 注释): 会话级 Cookie 随浏览器
					// 关闭失效, 用户重开浏览器就会撞上提示页。
					if strings.TrimSpace(r.URL.Query().Get("token")) != "" {
						http.SetCookie(w, &http.Cookie{
							Name:     adminTokenCookie,
							Value:    token,
							Path:     "/",
							MaxAge:   adminCookieMaxAge,
							HttpOnly: true,
							SameSite: http.SameSiteStrictMode,
						})
					}
				} else {
					// 带了令牌但校验没过: 不是"没令牌", 要明确告诉用户令牌无效并回填输入,
					// 别让他重新手抄一遍(见 renderAdminTokenPrompt)。
					mismatch = true
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if ok {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(webui.HTML))
			return
		}
		// 无令牌 / 令牌失效: 返回小提示页。mismatch=true 时提示页显示"访问令牌无效"并回填输入
		// (非 401, 否则双击 exe 后浏览器首次访问看不到任何内容)。
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(renderAdminTokenPrompt(mismatch, provided)))
		return
	}
	http.NotFound(w, r)
}
