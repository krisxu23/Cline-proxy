package app

import (
	"bytes"
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// uidSeq newUID 的进程内序号, 见 newUID。
var uidSeq int64

// newUID 生成带前缀的唯一 ID。
// 原来用毫秒时间戳作 ID: 同毫秒并发创建即碰撞(P2)—— 两个面板标签页同时导入
// 得到同一 AccountID, getAccountByID/removeAccount 首个匹配就返回/删除, 操作错
// 账号且第二个永远点不掉; 同毫秒两次 OAuth start 也会互相顶掉对方的会话。
// 时间戳 + 进程内原子序号在进程内必不重复, 重启之间由时间戳前进保证。
func newUID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMilli(), atomic.AddInt64(&uidSeq, 1))
}

// sanitizeEmail 入库前把 email 里的换行换成空格。
// 含 \r\n 的 email 会经 refresh-all 等日志点伪造日志行(日志注入, P3)——
// 在入口统一清洗, 取代逐个打印点各自处理。
func sanitizeEmail(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
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
	req.Email = sanitizeEmail(req.Email)

	acc := &Account{
		AccountID:    newUID("acc"),
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
	// email 已在入口 sanitizeEmail 统一去换行(日志注入, P3-10);%q 兜底引号包裹。
	log.Printf("Account added via API: %q", req.Email)

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
		// 破坏性操作留痕(审计 P3): 谁删了哪个账号。
		log.Printf("Account deleted: %s", req.AccountID)
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

	sessionID := newUID("oauth")
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
			expireOAuthSessionLater(sessionID, state)
			return
		}

		reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			expireOAuthSessionLater(sessionID, state)
			return
		}

		email := "unknown"
		if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
			email = reg.Data.UserInfo.Email
		}
		email = sanitizeEmail(email)

		acc := &Account{
			AccountID:    newUID("acc"),
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
		expireOAuthSessionLater(sessionID, state)
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
	st, ok := oauthSessions[sessionID]
	// 锁内做值快照再解锁使用: 写侧 goroutine 持锁改 st 的同一批字段,
	// 解锁后再读 state.Done/Success/Email/Error 就是数据竞争(P2-13)。
	var done, success bool
	var email, errMsg string
	if ok {
		done, success, email, errMsg = st.Done, st.Success, st.Email, st.Error
	}
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    done,
		"success": success,
	}
	if done {
		resp["email"] = email
		if !success {
			resp["error"] = errMsg
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// oauthSessionTTL 完成后的 OAuth 登录会话在 map 里的保留时长。
// 轮询端要能读到最终结果(刚完成就删会让 status 轮询撞上 404),
// 但条目不能只增不减 —— 内存缓慢泄漏且 sessionId 长期可查(P2-13)。
const oauthSessionTTL = 5 * time.Minute

// expireOAuthSessionLater 完成态写入后安排 TTL 到期删除该会话条目。
// 用指针身份校验: 同一 sessionID 若已被新条目顶替, 不动它。
func expireOAuthSessionLater(id string, st *oauthSessionState) {
	time.AfterFunc(oauthSessionTTL, func() {
		oauthSessionsMu.Lock()
		if oauthSessions[id] == st {
			delete(oauthSessions, id)
		}
		oauthSessionsMu.Unlock()
	})
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
	// email 入口统一清洗: failEmail 错误行与入库共用同一来源(P3)。
	req.Email = sanitizeEmail(req.Email)

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
	// 错误信息只带行号与 email, 不带 token 任何片段 —— 对照 proxy.go 不打印
	// token 的既有约定, 前 16 字符进 API 响应同样算凭据外泄(P3-11)。
	failEmail := req.Email
	if failEmail == "" {
		failEmail = "unknown"
	}

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := cline.RefreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("line %d (email %s): %v", i+1, failEmail, err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    newUID("acc"),
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
		t.Email = sanitizeEmail(t.Email)
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
			AccountID:    newUID("acc"),
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
	p := loadPool()
	poolMu.Lock()
	n := len(p.Accounts)
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	// 破坏性操作留痕(审计 P3): 清掉多少、何时清。
	log.Printf("All accounts deleted (%d)", n)
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
	// acc 是 getAccountByID 返回的池内活指针, 写方(refreshAccountToken /
	// markAccountCooldown / pickAccount / bumpUsage)全在 poolMu 内改同批字段;
	// 锁内快照再解锁使用 —— Status/CooldownUntil 是多字字段, 裸读会 torn read(P1)。
	poolMu.Lock()
	prevStatus := acc.Status
	poolMu.Unlock()

	// 取 token（expired/cooldown 也尝试刷新，测试按钮不因状态直接拒绝）
	token, err := ensureAccountToken(acc)
	if err != nil {
		poolMu.Lock()
		acc.LastReason = "token refresh failed: " + err.Error()
		acc.Status = "expired"
		acc.CooldownUntil = time.Time{}
		reason := acc.LastReason
		// savePoolLocked 已释放 poolMu(其契约: 调用后不得再 Unlock)——
		// 此前多出的一次 Unlock 触发 "sync: unlock of unlocked mutex" 直接 fatal(P0)。
		savePoolLocked()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     reason,
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
		// markAccountCooldown 释放锁后不再裸读共享字段(P1): 锁内快照结果。
		poolMu.Lock()
		reason, until := acc.LastReason, acc.CooldownUntil
		poolMu.Unlock()
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        reason,
			"cooldownUntil": until.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(until)),
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
		// 同上: markAccountCooldown 已释放锁, 锁内快照再读(P1)。
		poolMu.Lock()
		why, until := acc.LastReason, acc.CooldownUntil
		poolMu.Unlock()
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        why,
			"cooldownUntil": until.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(until)),
			"httpStatus":    resp.StatusCode,
		}, "cooldown"
	}

	if resp.StatusCode == 401 {
		poolMu.Lock()
		acc.Status = "expired"
		acc.LastReason = "401 unauthorized"
		acc.CooldownUntil = time.Time{}
		reason := acc.LastReason
		// savePoolLocked 已释放 poolMu, 不得再 Unlock(P0)。
		savePoolLocked()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     reason,
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
