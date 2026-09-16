package app

import (
	"cline-go-proxy/internal/webui"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 面板里每个 api(...) 调用点都必须能在管理 mux 上找到真实路由。
// 资产本身已抽到 internal/webui(纯字符串常量), 但"面板调用的路径 == 服务端
// 注册的路由"这条不变量横跨两个包, 所以校验收在 app 包内。
func TestAdminPanelPathsResolveToHandlers(t *testing.T) {
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	re := regexp.MustCompile(`api\('(?:GET|POST)',\s*'([^']+)'`)
	matches := re.FindAllStringSubmatch(webui.HTML, -1)
	if len(matches) == 0 {
		t.Fatal("no api() call sites found in the admin UI string")
	}
	for _, m := range matches {
		path := m[1]
		if strings.Contains(path, "${") {
			continue // 动态拼接的路径无法静态校验
		}
		full := "/admin/api" + path
		_, pattern := mux.Handler(httptest.NewRequest("GET", full, nil))
		if pattern == "/" || pattern == "/admin/" {
			t.Errorf("panel calls %s but the admin mux has no route for it (matched %q)", full, pattern)
		}
	}
}
