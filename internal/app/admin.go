package app

import (
	"bytes"
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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
			w.Write([]byte(adminHTML))
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

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	accounts := ListAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":  accounts,
			"total":     len(accounts),
			"poolIndex": poolSnapshot().CurrentIdx,
		},
	})
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken is required"})
		return
	}

	// Validate by refreshing
	resp, err := cline.RefreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
		return
	}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", len(poolSnapshot().Accounts)+1)
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := cline.PollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
			email = reg.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: reg.Data.RefreshToken,
			AccessToken:  "workos:" + reg.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(reg.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", email)
	}()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    state.Done,
		"success": state.Success,
	}
	if state.Done {
		resp["email"] = state.Email
		if !state.Success {
			resp["error"] = state.Error
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := cline.RefreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", kit.Truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []struct {
			RefreshToken string `json:"refreshToken"`
			Email        string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
		return
	}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		if t.RefreshToken == "" {
			continue
		}
		resp, err := cline.RefreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		email := t.Email
		if email == "" {
			email = fmt.Sprintf("batch_%d", time.Now().UnixMilli())
		}
		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	accounts := append([]*Account(nil), p.Accounts...)
	poolMu.Unlock()
	for _, a := range accounts {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
// 检测限流并解除：向上游发送探测请求。若上游仍限流（429）则保持冷却，
// 重置无效；若探测成功则清除冷却、恢复正常状态，并重置今日统计。
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)

	if status == "active" {
		// 探测通过：解除冷却并重置今日统计
		resetTodayUsage(acc)
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: "检测通过：上游未限流，已解除冷却并重置今日统计",
			Data:    result,
		})
		return
	}

	// 仍限流/失效：保持冷却，重置无效
	msg := "上游仍限流，重置无效，保持冷却"
	if status == "expired" {
		msg = "Token 已失效，重置无效"
	} else if status == "error" {
		msg = "探测异常，请稍后重试"
	}
	if until, ok := result["cooldownUntil"].(string); ok && until != "" {
		msg += "（预计恢复 " + until + "）"
	}
	if remaining, ok := result["remaining"].(string); ok && remaining != "" {
		msg += "（剩余 " + remaining + "）"
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: false,
		Message: msg,
		Data:    result,
	})
}

// POST /admin/api/accounts/test  body: { accountId }
// 用指定账号发送一个 max_tokens=1 的极小探测请求，验证该账号是否可用。
// 如果命中 429/INFERENCE_CAP_ERROR，自动标记冷却并返回预计恢复时间。
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)
	reason, _ := result["reason"].(string)
	log.Printf("Test account %s: status=%s reason=%s", truncateEmail(acc.Email), status, reason)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: status == "active",
		Message: status,
		Data:    result,
	})
}

// testAccount 对单个账号执行轻量探测请求，返回详细结果与最终状态。
// 测试按钮是"升级版重置"：无论账号当前是 active/cooldown/expired，
// 都会尝试刷新 Token 并发起一次真实探测；成功则清除所有异常状态。
// 返回的 status: active / cooldown / expired / error
func testAccount(acc *Account) (map[string]any, string) {
	prevStatus := acc.Status
	prevCooldownUntil := acc.CooldownUntil
	_ = prevCooldownUntil

	// 取 token（expired/cooldown 也尝试刷新，测试按钮不因状态直接拒绝）
	token, err := ensureAccountToken(acc)
	if err != nil {
		poolMu.Lock()
		acc.LastReason = "token refresh failed: " + err.Error()
		acc.Status = "expired"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     acc.LastReason,
			"prevStatus": prevStatus,
		}, "expired"
	}

	// 构造极小探测请求：max_tokens=1, 单条用户消息。探测请求需与正常代理请求
	// 使用相同的模型选择、流式策略和任务 ID，否则部分模型会返回空响应。
	probeModel := getDefaultModel()
	sessionID := fmt.Sprintf("test_%d", time.Now().UnixMilli())
	probeBody := map[string]any{
		"model":            probeModel,
		"max_tokens":       1,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	if modelNeedsStream(probeModel) {
		probeBody["stream"] = true
	}
	bodyJSON, _ := json.Marshal(probeBody)

	req, err := http.NewRequest("POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    "error",
			"reason":    "build request: " + err.Error(),
		}, "error"
	}
	req.Header = clineHeaders(token, sessionID)
	// 账号探测也走网关统一出口: 否则在节点模式下会拿"直连结果"去判断账号好坏,
	// 结论与实际调用路径不一致。
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer probeCancel()
	req = req.WithContext(probeCtx)

	resp, err := getZenHTTPClient().Do(req)
	if err != nil {
		// 网络错误：5 分钟短冷却
		markAccountCooldown(acc, "network error: "+err.Error(), 5*time.Minute)
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        acc.LastReason,
			"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
		}, "cooldown"
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	if resp.StatusCode == 429 {
		duration := parseInferenceCapDuration(bodyStr)
		if duration <= 0 {
			duration = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		reason := kit.Truncate(bodyStr, 500)
		markAccountCooldown(acc, "429: "+reason, duration)
		log.Printf("Test hit 429 on %s, cooldown %v", truncateEmail(acc.Email), duration)
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        acc.LastReason,
			"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
			"httpStatus":    resp.StatusCode,
		}, "cooldown"
	}

	if resp.StatusCode == 401 {
		poolMu.Lock()
		acc.Status = "expired"
		acc.LastReason = "401 unauthorized"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     acc.LastReason,
			"httpStatus": resp.StatusCode,
		}, "expired"
	}

	if resp.StatusCode != 200 {
		// 其它错误：不强制冷却，按一次失败处理
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "error",
			"reason":     fmt.Sprintf("API %d: %s", resp.StatusCode, kit.Truncate(bodyStr, 300)),
			"httpStatus": resp.StatusCode,
		}, "error"
	}

	// 成功：清除所有异常状态（冷却/过期/原因），并递增使用计数
	poolMu.Lock()
	acc.Status = "active"
	acc.LastReason = ""
	acc.CooldownUntil = time.Time{}
	poolMu.Unlock()
	bumpUsage(acc)
	return map[string]any{
		"accountId":  acc.AccountID,
		"email":      acc.Email,
		"status":     "active",
		"reason":     "ok",
		"httpStatus": resp.StatusCode,
		"prevStatus": prevStatus,
	}, "active"
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 && days == 0 && hours == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// Global proxy config (mutable via API)
var (
	proxyConfig   = loadProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
	// HeadersAuto 打开后由后台定期对齐官方 Cline CLI 的版本类请求头。
	HeadersAuto bool `json:"headersAuto"`
	// HeadersSyncedAt / HeadersAutoVersion 最近一次自动对齐的结果(展示用)。
	HeadersSyncedAt    int64  `json:"headersSyncedAt,omitempty"`
	HeadersAutoVersion string `json:"headersAutoVersion,omitempty"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.50",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.50",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.50",
			"X-CORE-VERSION":     "0.0.70",
		},
	}
}

func proxyConfigFile() string { return kit.ResolveDataPath(".proxy-config.json") }

// loadProxyConfig 读取持久化的客户端配置, 缺失或损坏时退回默认值。
// 面板上改过的请求头必须跨重启保留, 否则每次重启都会悄悄回到内置默认值。
//
// 解析失败不能"部分采用": json.Unmarshal 不是事务性的, 半截 JSON 会留下已经
// 解出来的字段、丢掉其余部分, 得到的是一份"看着正常但少了东西"的配置, 而且
// 面板上看不出任何异常。这里改成解析失败就整体退回默认值, 并把坏文件改名留证。
func loadProxyConfig() *proxyConfigData {
	path := proxyConfigFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultProxyConfig() // 首次运行没有文件是正常情况
	}
	next := defaultProxyConfig()
	if err := json.Unmarshal(data, next); err != nil {
		log.Printf("proxy config parse failed (%s): %v; 退回默认配置", path, err)
		quarantineBadConfig(path)
		return defaultProxyConfig()
	}
	if next.Headers == nil {
		next.Headers = map[string]string{}
	}
	if next.Strategy == "" {
		next.Strategy = "round_robin"
	}
	return next
}

// quarantineBadConfig 把解析不了的配置文件改名留证, 而不是删除 ——
// 用户往往需要从里面手工捞回请求头之类的内容。
func quarantineBadConfig(path string) {
	bak := path + ".bad-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, bak); err != nil {
		log.Printf("  保留坏配置失败(%v), 原文件仍在 %s", err, path)
		return
	}
	log.Printf("  坏配置已保留为 %s", bak)
}

func saveProxyConfig() {
	proxyConfigMu.Lock()
	data, err := json.MarshalIndent(proxyConfig, "", "  ")
	proxyConfigMu.Unlock()
	if err != nil {
		return
	}
	// 全仓此前唯一的"先截断再写"非原子写: 写到一半被强杀会留下半个 JSON, 下次
	// 启动解析失败 -> 配置静默丢失。改用原子写(临时文件 + fsync + rename)。
	if err := kit.WriteFileAtomicDefault(proxyConfigFile(), data); err != nil {
		log.Printf("proxy config save failed: %v", err)
	}
}

// getProxyConfig 返回当前客户端配置的深拷贝。
//
// 与 getZenConfig 同理: 调用方包含每个 cline 上游请求(clineHeaders 会遍历
// Headers)与选账号热路径, 它们在锁外持有引用; 返回裸指针时后台的请求头
// 自动同步一写就与这些遍历并发访问同一张 map。
func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig.clone()
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	proxyConfig = c.clone()
	proxyConfigMu.Unlock()
	saveProxyConfig()
}

// mutateProxyConfig 是 proxyConfig 的唯一写入口, 语义同 mutateProvidersConfig:
// 锁内克隆 → 回调改克隆 → 整体替换 → 落盘。回调内不得再读配置(会自锁)。
func mutateProxyConfig(fn func(cfg *proxyConfigData)) {
	proxyConfigMu.Lock()
	next := proxyConfig.clone()
	if next == nil {
		next = defaultProxyConfig()
	}
	fn(next)
	proxyConfig = next
	proxyConfigMu.Unlock()
	saveProxyConfig()
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	snap := poolSnapshot()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": snap.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	// 此前用 UnixMilli/UnixNano 拼令牌, 时间可预测、相邻请求极易被猜出。
	// 改为 32 字节密码学随机 + hex, 不可预测。
	kb := make([]byte, 32)
	if _, err := rand.Read(kb); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "生成密钥失败: " + err.Error()})
		return
	}
	key := "cline_" + hex.EncodeToString(kb)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	address := r.Host
	if address == "" {
		address = proxyListenAddress
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address": address,
		// listenAddr 是真实绑定地址(可能是 0.0.0.0), apiBase 是客户端应使用的本机入口。
		"listenAddr":   proxyListenAddress,
		"apiBase":      localOrigin(),
		"strategy":     cfg.Strategy,
		"version":      buildVersion,
		"poolPath":     poolPathValue(),
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
		"headersAuto":  cfg.HeadersAuto,
		"headersSync": map[string]any{
			"auto":      cfg.HeadersAuto,
			"syncedAt":  cfg.HeadersSyncedAt,
			"version":   cfg.HeadersAutoVersion,
			"source":    clineRegistrySources[0],
			"intervalM": int(headersAutoSyncInterval / time.Minute),
		},
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel string            `json:"defaultModel"`
		HeadersAuto  *bool             `json:"headersAuto"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	cfg := getProxyConfig()
	changed := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		// 面板以「整表替换」提交: 先清掉已删除的键, 否则被删掉的头会一直残留。
		next := make(map[string]string, len(req.Headers))
		for k, v := range req.Headers {
			if strings.TrimSpace(k) == "" {
				continue
			}
			next[strings.TrimSpace(k)] = v
		}
		cfg.Headers = next
		changed = true
	}

	if req.HeadersAuto != nil {
		cfg.HeadersAuto = *req.HeadersAuto
		changed = true
	}

	if req.DefaultModel != "" {
		initModelsCache()
		modelsMu.Lock()
		_, ok := modelsCache[req.DefaultModel]
		modelsMu.Unlock()
		if !ok {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown model: " + req.DefaultModel})
			return
		}
		setDefaultModel(req.DefaultModel)
		changed = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":    cfg.Strategy,
		"headers":     cfg.Headers,
		"headersAuto": cfg.HeadersAuto,
		// 走 getDefaultModel() 而不是直接读包级变量: 后者由 modelsMu 保护,
		// 直接读会与并发的 setDefaultModel 构成数据竞争。
		"defaultModel": getDefaultModel(),
	}})
}

// POST /admin/api/config/headers/sync — 对齐官方 Cline CLI 的请求头版本
func handleAdminHeadersSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), headersSyncTimeout*2)
	defer cancel()
	headers, info, err := syncOfficialHeaders(ctx)
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: "无法获取官方版本: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"headers": headers,
		"cli":     info.CLI,
		"core":    info.Core,
		"source":  info.Source,
		"version": versionLabel(info),
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	ensureModelsFresh()
	// modelsSyncStamp 带锁读 lastSync: 同步协程在持锁状态下写它,
	// 直接裸读会与 /models/refresh 并发竞争。
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   getFreeModels(),
		"lastSync": modelsSyncStamp(),
	}})
}

// POST /admin/api/models/refresh
func handleAdminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initModelsCache()
	modelsMu.Lock()
	syncing := modelsSyncing
	modelsMu.Unlock()
	if syncing {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "sync already running"})
		return
	}
	go syncModelsOnce()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model sync started"})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	snap := poolSnapshot()
	active, cooldown, expired := 0, 0, 0
	for _, a := range snap.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    len(snap.Accounts),
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": "round_robin",
			"version":  buildVersion,
		},
	})
}

// GET /admin/api/accounts/export 导出全部账号 refreshToken（JSON 文件下载）
func handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	snap := poolSnapshot()
	items := make([]map[string]any, 0, len(snap.Accounts))
	for _, a := range snap.Accounts {
		items = append(items, map[string]any{
			"refreshToken": a.RefreshToken,
			"email":        a.Email,
		})
	}
	// 走统一的 apiResponse 信封, 前端 exportAccounts() 即可复用 api() 的错误处理/超时/中断,
	// 不再各自裸写 fetch。前端拿到 data 后自行构造 Blob 触发下载。
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: items})
}

// GET /admin/api/logs 最近请求日志（对话/调用历史）
// handleRequestLogs 请求日志查询(支持筛选与分页)。
//
// 查询参数:
//
//	q        关键词, 模糊匹配 id/model/resolvedModel/path/upstream/note/errMsg
//	upstream 精确匹配上游名(zen / cline / clinepass / provider/<name>)
//	status   "err"(>=400) | "ok"(<400) | 数字状态码
//	model    模型名子串(请求模型或实际模型)
//	page     页码, 从 1 起; pageSize 每页条数, 默认 50, 上限 500
//
// 返回 {logs, total, page, pageSize}: total 是筛选后的总数, 前端据此画分页。
func handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	upstream := strings.TrimSpace(r.URL.Query().Get("upstream"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	modelQ := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("model")))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 50
	}

	logs := LoadRequestLogs()
	// 倒序（最新在前）
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}

	filtered := make([]RequestLog, 0, len(logs))
	for _, l := range logs {
		if upstream != "" && l.Upstream != upstream && l.Route != upstream {
			continue
		}
		switch status {
		case "err":
			if l.Status < 400 {
				continue
			}
		case "ok":
			if l.Status >= 400 {
				continue
			}
		case "":
		default:
			if n, err := strconv.Atoi(status); err != nil || n != l.Status {
				continue
			}
		}
		if modelQ != "" &&
			!strings.Contains(strings.ToLower(l.Model), modelQ) &&
			!strings.Contains(strings.ToLower(l.ResolvedModel), modelQ) {
			continue
		}
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				l.ID, l.Model, l.ResolvedModel, l.Path, l.Upstream, l.Note, l.ErrMsg, l.ErrClass, l.Exit,
			}, "\x00"))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		filtered = append(filtered, l)
	}

	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageLogs := filtered[start:end]
	if pageLogs == nil {
		pageLogs = []RequestLog{}
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"logs":     pageLogs,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
}

// handleLogTrace 单条请求的路由决策轨迹(为什么选了这站/为什么跳过其它)。
// 轨迹只存路由元数据(TTL 30 分钟 / 最近 2000 条), 不含 prompt 与正文。
func handleLogTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	d := decisionTraceFor(id)
	if d == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "轨迹不存在或已过期(仅保留最近 30 分钟)"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"trace": d}})
}

// ============ 节点手动拉黑 (P2, 参照 easy_proxies 的手动黑名单) ============

// handleNodeBlacklist POST /admin/api/nodes/blacklist
// body: {"key": "<nodeLocalKey>", "hours": 24}  hours<=0 或缺省 = 长期
func handleNodeBlacklist(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Key   string  `json:"key"`
		Hours float64 `json:"hours"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body 必须是 {\"key\": \"...\", \"hours\": 24}"})
		return
	}
	blacklistNode(strings.TrimSpace(body.Key), time.Duration(body.Hours*float64(time.Hour)))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"blacklisted": body.Key, "list": blacklistList()}})
}

// handleNodeBlacklistClear POST /admin/api/nodes/blacklist/clear
// body: {"key": "..."} 或 {"all": true}
func handleNodeBlacklistClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Key string `json:"key"`
		All bool   `json:"all"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body)
	manualBlackMu.Lock()
	if body.All {
		manualBlack = map[string]time.Time{}
	} else if strings.TrimSpace(body.Key) != "" {
		delete(manualBlack, strings.TrimSpace(body.Key))
	}
	manualBlackMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"list": blacklistList()}})
}

// handleNodeBlacklistList GET /admin/api/nodes/blacklist
func handleNodeBlacklistList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"list": blacklistList()}})
}

// handleHealth 网关健康总览 (P2-20, 参照 OmniRoute 的 ProviderHealthAutopilotCard):
//
// 不是单一布尔, 而是 {state, score, signals, issues[]} —— 每条 issue 携带
// severity / recommendation / evidence, 让面板能回答"哪里不健康、为什么、
// 建议做什么"。全部信号来自现有冷却/健康/配置状态, 零额外请求。
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var issues []map[string]any
	score := 100
	addIssue := func(severity, component, problem, recommendation, evidence string) {
		issues = append(issues, map[string]any{
			"severity":       severity, // critical | warn | info
			"component":      component,
			"problem":        problem,
			"recommendation": recommendation,
			"evidence":       evidence,
		})
	}

	// 1) zen 主通道
	cfg := getZenConfig()
	zenReady := false
	if cfg.Enabled {
		if cfg.Key == "" {
			addIssue("critical", "zen", "opencode zen 未配置 key", "在「供应商管理」填写 zen API key, 或使用 public 匿名档", "zen.enabled=true 但 key 为空")
			score -= 30
		} else {
			zenReady = true
		}
	} else {
		addIssue("warn", "zen", "opencode zen 通道已停用", "如需使用 zen 免费模型, 在供应商管理里启用", "zen.enabled=false")
		score -= 10
	}

	// 2) cline 账号池
	if !clinePoolReady() {
		addIssue("warn", "cline", "cline 账号池当前无可用账号", "检查账号冷却状态或补充账号", "clinePoolReady()=false")
		score -= 15
	}

	// 3) ClinePass
	if !clinePassReady() {
		addIssue("info", "clinepass", "ClinePass 订阅池当前无可用 key", "如需 cline-pass/ 模型参与路由, 补充订阅 key", "clinePassReady()=false")
		score -= 5
	}

	// 4) 通用 Provider: 有目录但没 key 的要提醒
	for _, name := range providerNames() {
		pc, ok := providerConfigFor(name)
		if !ok {
			continue
		}
		if len(enabledAPIKeys(pc, name)) == 0 {
			if p := providerByName(name); p != nil && len(freeModelsFor(name, p)) > 0 {
				addIssue("warn", "provider/"+name, "供应商有可用模型但未配置 API Key", "在供应商管理里填写 "+name+" 的 key, 否则其模型不参与路由", "keys=0, models>0")
				score -= 10
			}
		}
	}

	// 5) 冷却/剔除/模型摘除计数
	candidateCoolMu.Lock()
	cooling, probing := 0, 0
	for _, c := range candidateCools {
		if time.Now().UnixMilli() < c.until {
			cooling++
		}
		if c.probing {
			probing++
		}
	}
	permanents := len(candidatePerms)
	candidateCoolMu.Unlock()
	if cooling > 0 {
		addIssue("info", "router", fmt.Sprintf("%d 个候选处于冷却", cooling), "冷却到期后半开探测会自动恢复; 持续冷却的候选请在路由页检查", fmt.Sprintf("cooling=%d", cooling))
	}
	if permanentlyRemoved := permanents; permanentlyRemoved > 0 {
		addIssue("info", "router", fmt.Sprintf("%d 个候选被永久剔除(无免费层/已下架)", permanentlyRemoved), "确认是否为预期行为; 误剔除可在路由页清空", fmt.Sprintf("permanent=%d", permanentlyRemoved))
	}

	// state 判定
	state := "ok"
	if score < 60 {
		state = "critical"
	} else if score < 90 {
		state = "warn"
	}
	if issues == nil {
		issues = []map[string]any{}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"state": state,
		"score": score,
		"signals": map[string]any{
			"zenReady":         zenReady,
			"clinePoolReady":   clinePoolReady(),
			"clinePassReady":   clinePassReady(),
			"cooling":          cooling,
			"probing":          probing,
			"permanentRemoved": permanents,
			"decisionTraces":   decisionTraceCount(),
		},
		"issues": issues,
	}})
}

// ============ 配置整体导出 / 导入 (P2-25) ============

// configBackupFiles 参与整体备份的配置文件(白名单: 不含日志/统计/缓存/令牌)。
// 注意其中两个含敏感凭据(refreshToken / API key), 导出文件务必妥善保管。
var configBackupFiles = []string{
	".zen-config.json",     // zen/上游/路由/压缩等主配置
	".proxy-config.json",   // 代理监听与网关 key
	".cline-accounts.json", // cline 账号池(含 refreshToken, 敏感)
	".clinepass-keys.json", // clinepass key 池(敏感)
	"node-regions.json",    // 出口地区探测缓存
}

// configBackupAllowed 文件名是否在备份白名单内(导入侧防路径穿越)。
func configBackupAllowed(name string) bool {
	for _, f := range configBackupFiles {
		if name == f {
			return true
		}
	}
	return false
}

// handleConfigExport GET /admin/api/config/export
// 把全部配置文件打包成一个 JSON(信封带 kind/version/exportedAt), 供换机/备份。
func handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	files := map[string]string{}
	for _, name := range configBackupFiles {
		if b, err := os.ReadFile(kit.ResolveDataPath(name)); err == nil && len(b) > 0 {
			files[name] = string(b)
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"kind":       "cline-proxy-config-backup",
		"version":    1,
		"exportedAt": time.Now().Format(time.RFC3339),
		"files":      files,
	}})
}

// handleConfigImport POST /admin/api/config/import
// body: {"files": {".zen-config.json": "<内容>", ...}}
//
// 安全与可靠性: 文件名必须命中白名单(防路径穿越); 内容必须是合法 JSON;
// 覆盖前把现有文件备份为 <name>.bak-import(一次性, 可人工回滚)。
// 写入后提示重启 —— 各配置在启动时加载, 热加载不在本功能范围内。
func handleConfigImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body 必须是 {\"files\": {\"<文件名>\": \"<内容>\"}}"})
		return
	}
	if len(body.Files) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "files 为空"})
		return
	}
	imported := make([]string, 0, len(body.Files))
	for name, content := range body.Files {
		if !configBackupAllowed(name) {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "文件名不在备份白名单内: " + name})
			return
		}
		if strings.TrimSpace(content) == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: name + " 内容为空"})
			return
		}
		var check any
		if err := json.Unmarshal([]byte(content), &check); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: name + " 不是合法 JSON: " + err.Error()})
			return
		}
	}
	// 全部校验通过才落盘(避免半套配置); 覆盖前备份现有文件。
	for name, content := range body.Files {
		path := kit.ResolveDataPath(name)
		if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
			_ = os.WriteFile(path+".bak-import", old, 0600)
		}
		if err := kit.WriteFileAtomicDefault(path, []byte(content)); err != nil {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "写入 " + name + " 失败: " + err.Error()})
			return
		}
		imported = append(imported, name)
	}
	log.Printf("  admin: 配置导入完成 (%d 个文件), 重启后生效", len(imported))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"imported": imported,
		"note":     "导入完成, 重启网关后生效; 原文件已备份为 <文件名>.bak-import",
	}})
}
