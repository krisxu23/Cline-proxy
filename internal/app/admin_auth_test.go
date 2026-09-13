package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// authProbe 是一个挂在 adminAuth 后面的假接口: 命中即 200。
func authProbe(w http.ResponseWriter, r *http.Request) {
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// 管理接口必须拒绝没有令牌的请求。
//
// 回归重点: /admin/api/* 此前是零鉴权的, 配合默认 0.0.0.0 与无条件
// Access-Control-Allow-Origin: *, 任意网页都能从浏览器里把
// /admin/api/accounts/export(全部账号 refreshToken) 读走。
func TestAdminAuthRejectsMissingToken(t *testing.T) {
	h := adminAuth(authProbe)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/admin/api/accounts", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401, 得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminAuthRejectsWrongToken(t *testing.T) {
	h := adminAuth(authProbe)
	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.Header.Set("X-Admin-Token", "definitely-not-the-token")

	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错令牌应 401, 得到 %d", rec.Code)
	}
}

func TestAdminAuthAcceptsTokenAndSetsCookie(t *testing.T) {
	token := loadOrCreateAdminToken()
	if token == "" {
		t.Fatal("应能生成访问令牌")
	}
	h := adminAuth(authProbe)

	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.Header.Set("X-Admin-Token", token)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确令牌应放行, 得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 通过后要种下 HttpOnly Cookie, 面板后续同源请求才能自动携带。
	cookies := rec.Result().Cookies()
	found := false
	for _, ck := range cookies {
		if ck.Name == adminTokenCookie && ck.Value == token {
			found = true
			if !ck.HttpOnly {
				t.Error("令牌 Cookie 必须是 HttpOnly, 否则页面脚本可读")
			}
		}
	}
	if !found {
		t.Fatalf("校验通过后应种下 %s Cookie, 实际: %v", adminTokenCookie, cookies)
	}
}

// 面板的实际工作方式: 带 ?token= 打开一次, 之后靠 Cookie。
func TestAdminAuthAcceptsCookie(t *testing.T) {
	token := loadOrCreateAdminToken()
	h := adminAuth(authProbe)

	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.AddCookie(&http.Cookie{Name: adminTokenCookie, Value: token})
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Cookie 令牌应放行, 得到 %d", rec.Code)
	}
}

// 公网来源的跨站请求必须被拒 —— 这正是「浏览器里打开任意网页就能读走凭据」
// 那条攻击链的入口。DNS rebinding 也会带公网 Origin。
func TestAdminAuthRejectsPublicOrigin(t *testing.T) {
	token := loadOrCreateAdminToken()
	h := adminAuth(authProbe)

	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("公网 Origin 应 403, 得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// 本机 / 局域网来源要放行: 面板同源的 POST 会带 Origin, 局域网直连面板同理。
func TestAdminAuthAllowsLocalOrigin(t *testing.T) {
	token := loadOrCreateAdminToken()
	h := adminAuth(authProbe)

	for _, origin := range []string{
		"http://127.0.0.1:3457",
		"http://localhost:3457",
		"http://192.168.1.10:3457",
	} {
		req := httptest.NewRequest("POST", "/admin/api/config/update", nil)
		req.Header.Set("X-Admin-Token", token)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Origin %s 应放行, 得到 %d: %s", origin, rec.Code, rec.Body.String())
		}
	}
}

// 非浏览器客户端(curl / 脚本 / 转发器)不带 Origin, 不应因此被拒。
func TestAdminAuthAllowsNoOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "/admin/api/stats", nil)
	if !originAllowed(req) {
		t.Fatal("无 Origin 的请求必须放行, 否则 curl / 脚本无法使用管理接口")
	}
}
