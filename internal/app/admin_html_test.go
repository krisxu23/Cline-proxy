package app

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestAdminPanelPathsResolveToHandlers(t *testing.T) {
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	re := regexp.MustCompile(`api\('(?:GET|POST)',\s*'([^']+)'`)
	matches := re.FindAllStringSubmatch(adminHTML, -1)
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
