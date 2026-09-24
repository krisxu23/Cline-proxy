package cline

// 2026-09-24 审查: OpenBrowser 的 url 来自上游返回的 verification_uri_complete,
// 不属于本进程可控内容 —— 必须白名单, 否则 file:// 可读本地文件。

import "testing"

func TestValidateBrowserURLOnlyAllowsHTTPS(t *testing.T) {
	ok := []string{
		"https://api.cline.bot/device?code=abc",
		"https://example.com/verify?user_code=XY",
	}
	for _, u := range ok {
		if err := validateBrowserURL(u); err != nil {
			t.Fatalf("应放行 %q: %v", u, err)
		}
	}

	bad := []string{
		"file:///C:/Windows/System32/drivers/etc/hosts",
		"file:///etc/passwd",
		"http://evil.example.com/verify", // 明文 http 也拒: 中间人可改目标
		"javascript:alert(1)",
		"ms-settings:privacy",
		"https://",  // 缺主机名
		"",          // 空
		"not a url", // 无 scheme
		"ftp://x/y", // 非 https
	}
	for _, u := range bad {
		if err := validateBrowserURL(u); err == nil {
			t.Fatalf("必须拒绝 %q", u)
		}
	}
}
