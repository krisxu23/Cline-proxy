package cline

import (
	"encoding/json"
	"fmt"
	"free-router/internal/kit"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	workosClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
	ClineAPIBase   = "https://api.cline.bot/api/v1"
)

// 端点基址。声明为变量是为了给测试留注入点(把 base 指向 httptest.Server, 校验
// 路径/方法/表单字段的组装), 生产路径不会改写它们。
// ClineAPIBase 仍是导出常量: app / providers 两个包直接引用它拼线上地址。
var (
	workosBaseURL = "https://api.workos.com"
	clineAPIBase  = ClineAPIBase
)

type deviceAuthResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type authenticateResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type clineAuthResp struct {
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
		UserInfo     *struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	} `json:"data"`
}

type clineRefreshResp struct {
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
	} `json:"data"`
}

func WorkosDeviceAuth() (*deviceAuthResp, error) {
	form := url.Values{"client_id": {workosClientID}}
	resp, err := kit.HTTPPostForm(workosBaseURL+"/user_management/authorize/device", form)
	if err != nil {
		return nil, fmt.Errorf("workos device auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body := kit.ReadBody(resp)
		return nil, fmt.Errorf("workos device auth failed: %d %s", resp.StatusCode, kit.Truncate(body, 200))
	}

	var d deviceAuthResp
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("workos device auth decode: %w", err)
	}
	return &d, nil
}

func PollWorkosToken(deviceCode string, interval, expiresIn int) (*authenticateResp, error) {
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	currentInterval := interval
	if currentInterval < 5 {
		currentInterval = 5
	}

	for time.Now().Before(deadline) {
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
			"client_id":   {workosClientID},
		}
		resp, err := kit.HTTPPostForm(workosBaseURL+"/user_management/authenticate", form)
		if err != nil {
			return nil, fmt.Errorf("workos poll: %w", err)
		}

		var a authenticateResp
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("workos poll decode: %w", err)
		}
		resp.Body.Close()

		if resp.StatusCode == 200 {
			return &a, nil
		}

		switch a.Error {
		case "authorization_pending":
			time.Sleep(time.Duration(currentInterval) * time.Second)
		case "slow_down":
			currentInterval += 5
			time.Sleep(time.Duration(currentInterval) * time.Second)
		default:
			errDesc := a.ErrorDesc
			if errDesc == "" {
				errDesc = a.Error
			}
			return nil, fmt.Errorf("workos polling error: %s", errDesc)
		}
	}
	return nil, fmt.Errorf("device authorization expired (timeout)")
}

func RegisterWithCline(workosAccess, workosRefresh string) (*clineAuthResp, error) {
	body := map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	}
	resp, err := kit.HTTPPostJSON(clineAPIBase+"/auth/register", body)
	if err != nil {
		return nil, fmt.Errorf("cline register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b := kit.ReadBody(resp)
		return nil, fmt.Errorf("cline register failed: %d %s", resp.StatusCode, kit.Truncate(b, 200))
	}

	var c clineAuthResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return nil, fmt.Errorf("cline register decode: %w", err)
	}
	return &c, nil
}

func RefreshClineToken(refreshToken string) (*clineRefreshResp, error) {
	body := map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}
	resp, err := kit.HTTPPostJSON(clineAPIBase+"/auth/refresh", body)
	if err != nil {
		return nil, fmt.Errorf("cline refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cline refresh failed: %d", resp.StatusCode)
	}

	var c clineRefreshResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return nil, fmt.Errorf("cline refresh decode: %w", err)
	}
	return &c, nil
}

func ParseExpiry(exp any) int64 {
	switch v := exp.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			return t.UnixMilli()
		}
		t, err = time.Parse(time.RFC3339Nano, v)
		if err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func OpenBrowser(url string) error {
	// 白名单(2026-09-24 审查): url 来自上游返回的 verification_uri_complete,
	// 上游被劫持时可以直接给 file:// 或任意 scheme, 被 rundll32/xdg-open 打开即
	// 本地文件读取/命令触发面。只放行 https://。
	if err := validateBrowserURL(url); err != nil {
		return err
	}
	var cmd string
	var args []string

	switch {
	case IsWindows():
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		// Try common browser openers
		for _, candidate := range []string{"xdg-open", "open", "gnome-open"} {
			if _, err := os.Stat("/usr/bin/" + candidate); err == nil {
				cmd = candidate
				break
			}
			if _, err := os.Stat("/usr/local/bin/" + candidate); err == nil {
				cmd = candidate
				break
			}
		}
	}

	if cmd == "" {
		return fmt.Errorf("no browser opener found")
	}

	return kit.RunCommand(cmd, args...)
}

func IsWindows() bool {
	return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows")
}

// validateBrowserURL 只放行 https:// 的绝对 URL。
//
// 为什么必须白名单: 被打开的目标是**上游返回**的 verification_uri_complete,
// 不属于本进程可控内容。rundll32 url.dll,FileProtocolHandler 与 xdg-open 都会
// 忠实执行传进去的 scheme —— file:/// 可读本地文件, 自定义 scheme 可能触发别的
// 程序。只认 https 把面收窄到"网页"(2026-09-24 审查)。
func validateBrowserURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("浏览器地址解析失败: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("拒绝打开非 https 地址(只允许 https, 实际 %q)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("浏览器地址缺少主机名: %q", raw)
	}
	return nil
}
