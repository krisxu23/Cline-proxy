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

// 注: catalogModel 结构体声明在 providers_config.go, 此处不要重复声明。

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

// evalProviderFree 免费判定四模式(纯函数):
//   - catalog+allModels: 目录里的聊天模型全部可用(通用 Provider 的默认形态)
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
	if cfg.Catalog && cfg.AllModels {
		if len(cat) == 0 {
			// 目录尚未拉到(或该上游不提供 /models): 退回白名单, 保住手填的模型名,
			// 避免首次连接或上游无目录时全部模型被判死。
			return cfg.freeSet()[modelID]
		}
		m := catalogLookup(cat, slugs, modelID)
		return m != nil && isChatModel(m)
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
	providerCatalogRefresh = 15 * time.Minute
	// providerCatalogRetryMs 请求路径刷新失败后的重试间隔(1 分钟):
	// 目录为空的 provider 可以较快重试, 又不会被每个请求反复打上游。
	providerCatalogRetryMs  = int64(time.Minute / time.Millisecond)
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
	if custom := cfg.customModelsURL(); custom != "" {
		return custom
	}
	if isGoogleProvider(cfg) {
		return googleOpenAIBase(cfg.BaseURL) + cfg.modelsPath()
	}
	return strings.TrimRight(cfg.BaseURL, "/") + cfg.modelsPath()
}

// catalogHeaders 目录请求的鉴权头。
// 默认 Bearer; 显式配了 ModelsKeyHeader 时该头替代 Bearer(历史行为)。
// Google 例外: OpenAI 兼容目录认 Bearer, 原生目录认 x-goog-api-key,
// 按 target 路径择一发送 —— 原生目录上多发一个 Bearer 会被回 401,
// 反而盖住真实原因(地区不受支持等)。
func catalogHeaders(cfg providerConfig, target string) map[string]string {
	if cfg.APIKey == "" {
		return nil
	}
	google := isGoogleProvider(cfg)
	if cfg.ModelsKeyHeader != "" && !google {
		return map[string]string{cfg.ModelsKeyHeader: cfg.APIKey}
	}
	out := map[string]string{}
	if cfg.ModelsKeyHeader != "" {
		out[cfg.ModelsKeyHeader] = cfg.APIKey
	}
	cfg.applyAuth(target, func(k, v string) { out[k] = v })
	return out
}

// nativeCatalogParams 该目录地址是否使用 Google 原生分页参数。
// 自定义目录地址(尤其 Google 原生 /v1beta/models)才带 pageSize;
// OpenAI 兼容端点忽略未知查询参数, 但没必要无谓地加。
func nativeCatalogParams(rawURL string) bool {
	return !strings.Contains(strings.ToLower(rawURL), "/openai")
}

func (p *modelProvider) fetchCatalogPage(ctx context.Context, cfg providerConfig, rawURL, pageToken string) (catalogPage, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return catalogPage{}, err
	}
	q := u.Query()
	if nativeCatalogParams(rawURL) {
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
	for k, v := range catalogHeaders(cfg, u.String()) {
		req.Header.Set(k, v)
	}
	resp, err := providerExitClient().Do(req)
	if err != nil {
		return catalogPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return catalogPage{}, &catalogHTTPError{status: resp.StatusCode, body: string(body)}
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

// fetchCatalogPages 逐页拉取一个目录地址直到取完。
//
// 与对话请求同一套出口自愈: 网络错误或出口地区被拒时, 冷却刚失败的节点
// 换下一个重试 —— 节点池里"对这个上游不通"的节点不该卡死目录抓取。
// 直连模式没有第二个出口可换, 失败原样返回。
func (p *modelProvider) fetchCatalogPages(ctx context.Context, cfg providerConfig, rawURL string) ([]*catalogModel, error) {
	var listed []*catalogModel
	pageToken := ""
	for page := 0; page < providerCatalogMaxPages; page++ {
		pg, err := p.fetchCatalogPage(ctx, cfg, rawURL, pageToken)
		if err == nil {
			listed = append(listed, pg.models...)
			pageToken = pg.nextPageToken
			if pageToken == "" {
				break
			}
			continue
		}
		// 第一页就碰上出口问题才有换节点的意义: 后续页失败多半是上游本身,
		// 此时整页重试也拿不到完整目录。
		if page == 0 && !exitModeDirectNow() && p.retryCatalogOnNextExit(ctx, cfg, rawURL, err) {
			return p.fetchCatalogPages(ctx, cfg, rawURL)
		}
		return nil, err
	}
	return listed, nil
}

// retryCatalogOnNextExit 目录抓取的换出口判定。网络错误或出口地区被拒时,
// 冷却当前出口并返回 true 让调用方重试; 重试预算用尽返回 false。
func (p *modelProvider) retryCatalogOnNextExit(ctx context.Context, cfg providerConfig, rawURL string, err error) bool {
	if p.catalogExitRetries >= providerExitRetries {
		return false
	}
	status, body := 0, []byte(nil)
	if pe, ok := err.(*providerError); ok {
		status, body = pe.Status, []byte(pe.Body)
	} else if ce, ok := err.(*catalogHTTPError); ok {
		status, body = ce.status, []byte(ce.body)
	}
	regionRejected := status != 0 && isExitRegionRejected(status, body)
	if status != 0 && !regionRejected {
		return false // HTTP 层错误(4xx/5xx)换节点无意义, 只有连接层/地区拒绝才换
	}
	p.catalogExitRetries++
	reason := fmt.Sprintf("network error (%v)", err)
	if regionRejected {
		reason = fmt.Sprintf("exit region rejected: %s", kit.Truncate(string(body), 160))
	}
	rotateProviderExit(p.name, "catalog "+reason, p.catalogExitRetries)
	return true
}

// catalogHTTPError 把 fetchCatalogPage 的非 200 带结构化状态码, 供换出口判定。
type catalogHTTPError struct {
	status int
	body   string
}

func (e *catalogHTTPError) Error() string {
	return fmt.Sprintf("catalog HTTP %d: %s", e.status, kit.Truncate(e.body, 300))
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
	p.catalogExitRetries = 0
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, providerCatalogTimeout)
	defer cancel()

	// Google 有两套目录方言: OpenAI 兼容 /v1beta/openai/models(OpenAI 形状)与
	// 原生 /v1beta/models(带 pageToken 分页)。默认打兼容端点, 不被支持时退回原生,
	// 两种形状 normalizeCatalogPayload 都能解析 —— 因此 Base URL 怎么填都能拉到模型。
	urls := []string{catalogURL(cfg)}
	if isGoogleProvider(cfg) {
		if native := googleNativeCatalogURL(cfg); native != urls[0] {
			urls = append(urls, native)
		}
	}
	var (
		listed  []*catalogModel
		lastErr error
	)
	for _, cu := range urls {
		listed, lastErr = p.fetchCatalogPages(ctx, cfg, cu)
		if lastErr == nil {
			break
		}
		if len(urls) > 1 {
			log.Printf("  providers: %s catalog via %s failed (%v), trying the next endpoint", p.name, cu, lastErr)
		}
	}
	if lastErr != nil {
		p.mu.Lock()
		p.catalogErr = lastErr.Error()
		p.mu.Unlock()
		return lastErr
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
		// Google 目录条目带 models/ 前缀, 在入口处统一剥掉:
		// 之后查找/发布/回传上游都用同一个名字, 不会出现 models/gemini-... 这种前缀。
		m.ID = cfg.stripProviderModelPrefix(m.ID)
		if m.ID == "" {
			continue
		}
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
	switch {
	case cfg.Catalog && cfg.AllModels:
		// 目录里的聊天模型全部可用; 目录为空时退回白名单。
		for _, m := range cat {
			if isChatModel(m) {
				candidates = append(candidates, *m)
			}
		}
		if len(candidates) == 0 {
			for id := range cfg.freeSet() {
				candidates = append(candidates, catalogModel{ID: id})
			}
		}
	case cfg.Catalog && cfg.Pricing:
		for _, m := range cat {
			if isZeroCost(m) && isChatModel(m) {
				candidates = append(candidates, *m)
			}
		}
	default:
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

// startProviderRefresher 启动时立即刷新一次, 之后每 15 分钟刷新一次启用 catalog 的 provider。
func startProviderRefresher() {
	go func() {
		refreshOnce := func() {
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
		refreshOnce()
		t := time.NewTicker(providerCatalogRefresh)
		defer t.Stop()
		for range t.C {
			refreshOnce()
		}
	}()
}
