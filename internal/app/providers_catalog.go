package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

// 注: catalogModel 结构体已由 Task 1 在 providers_config.go 中声明, 此处不要重复声明。

// isZeroCost 明确标价 0 的模型。
func isZeroCost(m *catalogModel) bool {
	return m != nil && m.PricesKnown && m.PromptPrice == 0 && m.CompletionPrice == 0
}

var moderationRe = regexp.MustCompile(`(?i)content[-_ ]?safety|moderation|guard(?:[:/_-]|$)`)

// isChatModel 目录条目是否可作为聊天模型路由。
func isChatModel(m *catalogModel) bool {
	if m == nil {
		return false
	}
	if m.ChatCapable != nil {
		return *m.ChatCapable
	}
	outputs := m.OutputModalities
	if len(outputs) == 0 {
		outputs = []string{"text"}
	}
	for _, o := range outputs {
		if o != "text" {
			return false
		}
	}
	if m.Tokenizer == "Router" {
		return false
	}
	return !moderationRe.MatchString(m.ID)
}

// normalizeModelSlug 跨 provider 归一化模型名: 小写、剥 :free 后缀、取最后一段。
func normalizeModelSlug(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	s = strings.TrimSuffix(s, ":free")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// catalogLookup 精确命中优先, 否则按 slug 回退。
func catalogLookup(cat, slugs map[string]*catalogModel, modelID string) *catalogModel {
	if m, ok := cat[modelID]; ok {
		return m
	}
	slug := normalizeModelSlug(modelID)
	if slug == "" {
		return nil
	}
	return slugs[slug]
}

// evalProviderFree 免费判定三模式(纯函数):
//   - catalog+pricing: 目录按价格判定 isZeroCost && isChatModel
//   - 白名单: freeModels 命中; 目录已加载时还需目录中仍存在
//   - 永久拒绝缓存命中 → false
func evalProviderFree(cfg providerConfig, cat, slugs map[string]*catalogModel, rejected map[string]string, modelID string) bool {
	if cfg.APIKey == "" {
		return false
	}
	if _, bad := rejected[modelID]; bad {
		return false
	}
	if cfg.Catalog && cfg.Pricing {
		if len(cat) == 0 {
			return false
		}
		m := catalogLookup(cat, slugs, modelID)
		return m != nil && isZeroCost(m) && isChatModel(m)
	}
	if !cfg.freeSet()[modelID] {
		return false
	}
	if cfg.Catalog && len(cat) > 0 {
		return catalogLookup(cat, slugs, modelID) != nil
	}
	return true
}

// isFree 该模型在本 provider 上是否免费。
func (p *modelProvider) isFree(modelID string) bool {
	cfg, _ := providerConfigFor(p.name)
	p.mu.Lock()
	cat, slugs, rejected := p.catalog, p.slugs, p.rejected
	p.mu.Unlock()
	return evalProviderFree(cfg, cat, slugs, rejected, modelID)
}

// catalogEntry 精确或按 slug 查目录条目。
func (p *modelProvider) catalogEntry(modelID string) *catalogModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return catalogLookup(p.catalog, p.slugs, modelID)
}

const (
	providerCatalogRefresh  = 15 * time.Minute
	providerCatalogMaxPages = 10
	providerCatalogPageSize = "1000"
	providerCatalogTimeout  = 90 * time.Second
)

// catalogPage 一次目录响应的归一化结果。
type catalogPage struct {
	shape         string // "openai" / "google" / "unknown"
	models        []*catalogModel
	nextPageToken string
}

func parsePrice(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

type openaiCatalogPayload struct {
	Data []struct {
		ID            string `json:"id"`
		Created       int64  `json:"created"`
		ContextLength int    `json:"context_length"`
		Pricing       struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
		Architecture struct {
			OutputModalities []string `json:"output_modalities"`
			Tokenizer        string   `json:"tokenizer"`
		} `json:"architecture"`
	} `json:"data"`
}

type googleCatalogPayload struct {
	Models []struct {
		Name                       string   `json:"name"`
		DisplayName                string   `json:"displayName"`
		InputTokenLimit            int      `json:"inputTokenLimit"`
		OutputTokenLimit           int      `json:"outputTokenLimit"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// normalizeCatalogPayload 识别 OpenAI 兼容与 Google native 两种目录形状。
func normalizeCatalogPayload(payload []byte) catalogPage {
	var openai openaiCatalogPayload
	if json.Unmarshal(payload, &openai) == nil && len(openai.Data) > 0 {
		models := make([]*catalogModel, 0, len(openai.Data))
		for _, raw := range openai.Data {
			if raw.ID == "" {
				continue
			}
			pp, pOk := parsePrice(raw.Pricing.Prompt)
			cp, cOk := parsePrice(raw.Pricing.Completion)
			models = append(models, &catalogModel{
				ID:               raw.ID,
				Created:          raw.Created,
				ContextLength:    raw.ContextLength,
				OutputModalities: raw.Architecture.OutputModalities,
				Tokenizer:        raw.Architecture.Tokenizer,
				PricesKnown:      pOk && cOk,
				PromptPrice:      pp,
				CompletionPrice:  cp,
			})
		}
		return catalogPage{shape: "openai", models: models}
	}
	var google googleCatalogPayload
	if json.Unmarshal(payload, &google) == nil && len(google.Models) > 0 {
		models := make([]*catalogModel, 0, len(google.Models))
		for _, raw := range google.Models {
			if raw.Name == "" {
				continue
			}
			chat := false
			for _, m := range raw.SupportedGenerationMethods {
				if m == "generateContent" {
					chat = true
					break
				}
			}
			cc := chat
			models = append(models, &catalogModel{
				ID:            strings.TrimPrefix(raw.Name, "models/"),
				Name:          raw.DisplayName,
				ContextLength: raw.InputTokenLimit,
				MaxOutput:     raw.OutputTokenLimit,
				ChatCapable:   &cc,
			})
		}
		return catalogPage{shape: "google", models: models, nextPageToken: google.NextPageToken}
	}
	return catalogPage{shape: "unknown"}
}

func catalogURL(cfg providerConfig) string {
	if cfg.ModelsURL != "" {
		return cfg.ModelsURL
	}
	return strings.TrimRight(cfg.BaseURL, "/") + cfg.modelsPath()
}

func catalogHeaders(cfg providerConfig) map[string]string {
	if cfg.APIKey == "" {
		return nil
	}
	if cfg.ModelsKeyHeader != "" {
		return map[string]string{cfg.ModelsKeyHeader: cfg.APIKey}
	}
	return map[string]string{"Authorization": "Bearer " + cfg.APIKey}
}

func (p *modelProvider) fetchCatalogPage(ctx context.Context, cfg providerConfig, pageToken string) (catalogPage, error) {
	u, err := url.Parse(catalogURL(cfg))
	if err != nil {
		return catalogPage{}, err
	}
	q := u.Query()
	if cfg.ModelsURL != "" {
		q.Set("pageSize", providerCatalogPageSize)
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return catalogPage{}, err
	}
	for k, v := range catalogHeaders(cfg) {
		req.Header.Set(k, v)
	}
	resp, err := providerDirectClient.Do(req)
	if err != nil {
		return catalogPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return catalogPage{}, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, kit.Truncate(string(body), 300))
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return catalogPage{}, err
	}
	page := normalizeCatalogPayload(payload)
	if page.shape == "unknown" {
		return catalogPage{}, fmt.Errorf("unrecognised catalog response shape")
	}
	return page, nil
}

// refreshCatalog 拉取并重建目录; force 强制刷新, 否则受 15 分钟间隔限制。
func (p *modelProvider) refreshCatalog(ctx context.Context, force bool) error {
	cfg, _ := providerConfigFor(p.name)
	if !cfg.Catalog {
		return nil
	}
	p.mu.Lock()
	now := time.Now().UnixMilli()
	if !force && now-p.attemptedAt < int64(providerCatalogRefresh/time.Millisecond) &&
		(len(p.catalog) > 0 || p.catalogErr != "") {
		p.mu.Unlock()
		return nil
	}
	p.attemptedAt = now
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, providerCatalogTimeout)
	defer cancel()

	var listed []*catalogModel
	pageToken := ""
	for page := 0; page < providerCatalogMaxPages; page++ {
		pg, err := p.fetchCatalogPage(ctx, cfg, pageToken)
		if err != nil {
			p.mu.Lock()
			p.catalogErr = err.Error()
			p.mu.Unlock()
			return err
		}
		listed = append(listed, pg.models...)
		pageToken = pg.nextPageToken
		if pageToken == "" {
			break
		}
	}
	if len(listed) == 0 {
		err := fmt.Errorf("catalog response listed no models")
		p.mu.Lock()
		p.catalogErr = err.Error()
		p.mu.Unlock()
		return err
	}
	cat := make(map[string]*catalogModel, len(listed))
	slugs := make(map[string]*catalogModel, len(listed))
	for _, m := range listed {
		cat[m.ID] = m
		slug := normalizeModelSlug(m.ID)
		if slug != "" {
			if _, ok := slugs[slug]; !ok {
				slugs[slug] = m
			}
		}
	}
	p.mu.Lock()
	p.catalog = cat
	p.slugs = slugs
	p.fetchedAt = time.Now().UnixMilli()
	p.catalogErr = ""
	p.mu.Unlock()
	return nil
}

// freeModelIDs 该 provider 的免费模型列表。
// 候选统一经 evalProviderFree 过滤: 该列表会对外发布(providerModelList),
// 必须与 isFree 的判定一致, 缺 key 或已永久剔除的模型不得出现。
func (p *modelProvider) freeModelIDs() []catalogModel {
	cfg, _ := providerConfigFor(p.name)
	p.mu.Lock()
	cat, slugs, rejected := p.catalog, p.slugs, p.rejected
	p.mu.Unlock()

	var candidates []catalogModel
	if cfg.Catalog && cfg.Pricing {
		for _, m := range cat {
			if isZeroCost(m) && isChatModel(m) {
				candidates = append(candidates, *m)
			}
		}
	} else {
		for id := range cfg.freeSet() {
			candidates = append(candidates, catalogModel{ID: id})
		}
	}
	out := make([]catalogModel, 0, len(candidates))
	for _, m := range candidates {
		if evalProviderFree(cfg, cat, slugs, rejected, m.ID) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// providerModelList 全部已配置 provider 的免费模型, id 形如 "provider:model"。
func providerModelList() []map[string]any {
	out := []map[string]any{}
	for _, name := range providerNames() {
		p := providerByName(name)
		if p == nil {
			continue
		}
		cfg, _ := providerConfigFor(name)
		if cfg.APIKey == "" {
			continue
		}
		for _, m := range p.freeModelIDs() {
			out = append(out, map[string]any{
				"id":       name + ":" + m.ID,
				"object":   "model",
				"created":  time.Now().UnixMilli(),
				"owned_by": name,
				"source":   "provider",
				"status":   "active",
				"cost":     "free",
				"context":  m.ContextLength,
				"output":   m.MaxOutput,
			})
		}
	}
	return out
}

// catalogStatus 目录与配置状态(管理端展示)。
func (p *modelProvider) catalogStatus() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg, _ := providerConfigFor(p.name)
	freeCount, chatCount := 0, 0
	for _, m := range p.catalog {
		if isZeroCost(m) {
			freeCount++
		}
		if isChatModel(m) {
			chatCount++
		}
	}
	return map[string]any{
		"configured":  cfg.APIKey != "",
		"catalog":     cfg.Catalog,
		"catalogSize": len(p.catalog),
		"freeCount":   freeCount,
		"chatCount":   chatCount,
		"lastFetch":   p.fetchedAt,
		"error":       p.catalogErr,
		"rejected":    len(p.rejected),
	}
}

// startProviderRefresher 每 15 分钟刷新一次启用 catalog 的 provider。
func startProviderRefresher() {
	go func() {
		t := time.NewTicker(providerCatalogRefresh)
		defer t.Stop()
		for range t.C {
			for _, name := range providerNames() {
				p := providerByName(name)
				pc, ok := providerConfigFor(name)
				if p == nil || !ok || !pc.Catalog {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), providerCatalogTimeout)
				if err := p.refreshCatalog(ctx, false); err != nil {
					log.Printf("  providers: refresh %s failed: %v", name, err)
				}
				cancel()
			}
		}
	}()
}
