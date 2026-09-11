package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"cline-go-proxy/internal/kit"
)

// GET /admin/api/providers
func handleProvidersConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	providers := map[string]any{}
	for _, name := range providerNames() {
		cfg, _ := providerConfigFor(name)
		p := providerByName(name)
		runtime := map[string]any{"configured": cfg.APIKey != ""}
		if p != nil {
			runtime = p.catalogStatus()
		}
		providers[name] = map[string]any{
			"baseUrl":         cfg.BaseURL,
			"apiKey":          cfg.APIKey,
			"catalog":         cfg.Catalog,
			"pricing":         cfg.Pricing,
			"probeFreeTier":   cfg.ProbeFreeTier,
			"chatPath":        cfg.ChatPath,
			"modelsPath":      cfg.ModelsPath,
			"modelsUrl":       cfg.ModelsURL,
			"modelsKeyHeader": cfg.ModelsKeyHeader,
			"headers":         cfg.Headers,
			"freeModels":      cfg.FreeModels,
			"runtime":         runtime,
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"providers": providers}})
}

// POST /admin/api/providers/update  {name, provider?, remove?}
func handleProvidersUpdate(w http.ResponseWriter, r *http.Request) {
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
		Name     string          `json:"name"`
		Provider *providerConfig `json:"provider"`
		Remove   bool            `json:"remove"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	if req.Name == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "name is required"})
		return
	}
	if req.Remove {
		mutateProvidersConfig(func(cfg *zenConfigData) {
			if cfg.Providers != nil {
				delete(cfg.Providers, req.Name)
			}
		})
		providerRTMu.Lock()
		delete(providerRT, req.Name)
		providerRTMu.Unlock()
		writeAPI(w, http.StatusOK, apiResponse{Success: true})
		return
	}
	if req.Provider == nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "provider is required"})
		return
	}
	if err := validateProviderConfig(req.Name, *req.Provider); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	mutateProvidersConfig(func(cfg *zenConfigData) {
		if cfg.Providers == nil {
			cfg.Providers = map[string]providerConfig{}
		}
		cfg.Providers[req.Name] = *req.Provider
	})
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// POST /admin/api/providers/refresh  {name?}
func handleProvidersRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var req struct {
		Name string `json:"name"`
	}
	json.Unmarshal(body, &req)

	names := providerNames()
	if req.Name != "" {
		if providerByName(req.Name) == nil {
			writeAPI(w, http.StatusNotFound, apiResponse{Error: "unknown provider: " + req.Name})
			return
		}
		names = []string{req.Name}
	}
	go func() {
		for _, n := range names {
			p := providerByName(n)
			if p == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), providerCatalogTimeout)
			if err := p.refreshCatalog(ctx, true); err != nil {
				log.Printf("  providers: refresh %s failed: %v", n, err)
			} else {
				log.Printf("  providers: catalog refreshed (%s)", n)
			}
			cancel()
		}
	}()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"started": true}})
}

// POST /admin/api/providers/test  {name, model?}
func handleProvidersTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var req struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	p := providerByName(req.Name)
	if p == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "unknown provider: " + req.Name})
		return
	}
	cfg, _ := providerConfigFor(req.Name)
	if cfg.APIKey == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "provider is not configured"})
		return
	}
	model := req.Model
	if model == "" {
		if ids := p.freeModelIDs(); len(ids) > 0 {
			model = ids[0].ID
		}
	}
	if model == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "model is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	params := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	}
	resp, err := p.Chat(ctx, params, false)
	if err != nil {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
			"status": providerErrorStatus(err),
			"error":  err.Error(),
		}})
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"status": resp.StatusCode,
		"body":   kit.Truncate(string(b), 400),
	}})
}
