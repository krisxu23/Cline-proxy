package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminProvidersUpdateAndGet(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(
		`{"name":"openrouter","provider":{"baseUrl":"https://openrouter.ai/api/v1","apiKey":"k1","catalog":true,"pricing":true}}`))
	w := httptest.NewRecorder()
	handleProvidersUpdate(w, req)
	if w.Code != 200 {
		t.Fatalf("update status: %d body=%s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(`{"name":"openrouter","remove":true}`))
		handleProvidersUpdate(httptest.NewRecorder(), req)
	})

	getW := httptest.NewRecorder()
	handleProvidersConfig(getW, httptest.NewRequest("GET", "/admin/api/providers", nil))
	if getW.Code != 200 {
		t.Fatalf("get status: %d", getW.Code)
	}
	body := getW.Body.String()
	for _, want := range []string{`"openrouter"`, `"https://openrouter.ai/api/v1"`, `"baseUrl"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
}

func TestAdminProvidersUpdateRejectsInvalid(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(
		`{"name":"BadName","provider":{"baseUrl":"https://x"}}`))
	w := httptest.NewRecorder()
	handleProvidersUpdate(w, req)
	if w.Code == 200 {
		t.Fatal("invalid provider id must be rejected")
	}
	req2 := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(
		`{"name":"ok","provider":{"baseUrl":""}}`))
	w2 := httptest.NewRecorder()
	handleProvidersUpdate(w2, req2)
	if w2.Code == 200 {
		t.Fatal("missing baseUrl must be rejected")
	}
}

func TestAdminProvidersTestEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "t", providerConfig{BaseURL: srv.URL, APIKey: "k", Models: []providerModelEntry{{ID: "m1", Enabled: true}}})
	req := httptest.NewRequest("POST", "/admin/api/providers/test", strings.NewReader(`{"name":"t","model":"m1"}`))
	w := httptest.NewRecorder()
	handleProvidersTest(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":200`) {
		t.Fatalf("test endpoint: %d %s", w.Code, w.Body.String())
	}
}

// 删除 provider 必须同时清除运行时条目, 否则同名重新添加会复用旧设置的 modelProvider。
func TestAdminProvidersRemoveClearsRuntimeEntry(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(
		`{"name":"goneprov","provider":{"baseUrl":"https://gone.example/v1","apiKey":"k1"}}`))
	w := httptest.NewRecorder()
	handleProvidersUpdate(w, req)
	if w.Code != 200 {
		t.Fatalf("update status: %d body=%s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(`{"name":"goneprov","remove":true}`))
		handleProvidersUpdate(httptest.NewRecorder(), req)
	})
	if p := providerByName("goneprov"); p == nil {
		t.Fatal("provider must resolve after update")
	}

	rmReq := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(`{"name":"goneprov","remove":true}`))
	rmW := httptest.NewRecorder()
	handleProvidersUpdate(rmW, rmReq)
	if rmW.Code != 200 {
		t.Fatalf("remove status: %d body=%s", rmW.Code, rmW.Body.String())
	}
	if providerByName("goneprov") != nil {
		t.Fatal("removed provider must not stay configured")
	}
	providerRTMu.Lock()
	_, stale := providerRT["goneprov"]
	providerRTMu.Unlock()
	if stale {
		t.Fatal("remove must clear the runtime registry entry")
	}
}

// 改动 provider 设置必须丢弃按旧设置建立的运行时条目, 否则目录与连通测试会继续用旧端点。
func TestAdminProvidersUpdateClearsRuntimeEntry(t *testing.T) {
	setTestProvider(t, "editprov", providerConfig{BaseURL: "https://old.example/v1", APIKey: "k1"})
	before := providerByName("editprov")
	if before == nil {
		t.Fatal("provider must resolve after set")
	}

	req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(
		`{"name":"editprov","provider":{"baseUrl":"https://new.example/v1","apiKey":"k1"}}`))
	w := httptest.NewRecorder()
	handleProvidersUpdate(w, req)
	if w.Code != 200 {
		t.Fatalf("update status: %d body=%s", w.Code, w.Body.String())
	}

	after := providerByName("editprov")
	if after == nil {
		t.Fatal("provider must resolve after update")
	}
	if after == before {
		t.Fatal("update must drop the runtime entry built from the previous settings")
	}
}

// 刷新在后台执行: 未知名字同步拒绝, 已知名字立即返回 started。
func TestAdminProvidersRefreshStartsAsync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "asyncprov", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true})

	unknownW := httptest.NewRecorder()
	handleProvidersRefresh(unknownW, httptest.NewRequest("POST", "/admin/api/providers/refresh", strings.NewReader(`{"name":"nope"}`)))
	if unknownW.Code != 404 {
		t.Fatalf("unknown provider refresh should be 404: %d %s", unknownW.Code, unknownW.Body.String())
	}

	w := httptest.NewRecorder()
	handleProvidersRefresh(w, httptest.NewRequest("POST", "/admin/api/providers/refresh", strings.NewReader(`{"name":"asyncprov"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"started":true`) {
		t.Fatalf("refresh endpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminProvidersTestUnknown(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/test", strings.NewReader(`{"name":"nope"}`))
	w := httptest.NewRecorder()
	handleProvidersTest(w, req)
	if w.Code != 404 {
		t.Fatalf("unknown provider should be 404: %d %s", w.Code, w.Body.String())
	}
}

// 面板勾选列表的数据源: 全量目录 + 每个模型的启用状态(含被勾掉的)。
func TestAdminProvidersCatalogModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "cm", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true})
	p := providerByName("cm")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	got := p.catalogModels()
	if len(got) != 2 {
		t.Fatalf("catalogModels must list all catalog entries: %+v", got)
	}
	for _, m := range got {
		if m["disabled"] != false {
			t.Fatalf("default enabled: %+v", m)
		}
	}

	setTestProvider(t, "cm", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true,
		Models: []providerModelEntry{{ID: "m1", Enabled: true}, {ID: "m2", Enabled: false}}})
	got = providerByName("cm").catalogModels()
	seen := map[string]bool{}
	for _, m := range got {
		seen[m["id"].(string)] = m["disabled"].(bool)
	}
	if seen["m1"] != false || seen["m2"] != true {
		t.Fatalf("m2 must be disabled, m1 enabled: %+v", seen)
	}
}
