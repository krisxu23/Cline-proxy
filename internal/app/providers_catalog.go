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
//   - catalog+pricing: 目录按价格判定 isZeroCost && isChatModel;
//     目录完全无价格时退回白名单+目录校验(见 catalogHasPrices)
//   - 白名单: freeModels 命中; 目录已加载时还需目录中仍存在
//   - 永久拒绝缓存命中 → false
func evalProviderFree(cfg providerConfig, cat, slugs map[string]*catalogModel, rejected map[string]string, modelID string) bool {
	if cfg.APIKey == "" {
		return false
	}
	if _, bad := rejected[modelID]; bad {
		return false
	}
	if cfg.disabledSet()[modelID] {
		return false
	}
	if explicit, ok := cfg.explicitModels(); ok {
		return explicit[modelID]
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
		if !catalogHasPrices(cat) {
			// 无价格目录: 价格判据无效, 退回白名单+目录校验。
			if !cfg.freeSet()[modelID] {
				return false
			}
			return catalogLookup(cat, slugs, modelID) != nil
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

// catalogHasPrices 目录里是否至少有一条带价格的信息。B.AI 类上游的目录
// 完全不带价格, 此时价格判据恒为 false, 必须退回白名单语义。
func catalogHasPrices(cat map[string]*catalogModel) bool {
	for _, m := range cat {
		if m != nil && m.PricesKnown {
			return true
		}
	}
	return false
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

	// catalogExitMaxRotations 单次目录刷新的换出口次数硬上限。
	// 预算本身 = 池内健康出口数, 这里只是兜底: 万一冷却没生效(例如出口无法
	// 被冷却), 也不能让轮换无限进行。
	catalogExitMaxRotations = 16
	// catalogExitRotateMax 单次目录刷新的轮换总时长上限。
	// 订阅池几十上百个节点时, 即便只挑健康的也可能试很久, 而这段时间里
	// 管理面板与业务请求都要跟它抢出口。
	catalogExitRotateMax = 30 * time.Second
	// providerCatalogStartupDelay 启动后第一次目录刷新前的等待。
	// 订阅解析 / 节点实例重建 / 连通检测都在启动瞬间发生, 目录刷新晚一点起步,
	// 管理面板才不会刚打开就因出口被抢而"加载失败"。
	providerCatalogStartupDelay = 30 * time.Second
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

func (p *modelProvider) fetchCatalogPage(ctx context.Context, cfg providerConfig, rawURL, pageToken string, client *http.Client) (catalogPage, error) {
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
	resp, err := client.Do(req)
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

// catalogFallbackClient 目录抓取的兜底客户端。
//
// 走网关统一出口: 代理模式经节点(节点池全试一遍仍失败时, 由出口决策按
// "节点全挂兜底"开关决定是否直连), 直连模式经 sing-box 的 direct 出站。
// 原先这里是"强制直连"的旁路, 会绕过出口模式, 现已收回统一决策。
func catalogFallbackClient() *http.Client {
	return getZenHTTPClient()
}

// fetchCatalogPages 逐页拉取一个目录地址直到取完。第一页带出口轮换与直连兜底
// (fetchFirstCatalogPage), 后续页沿用第一页成功的那个客户端。
func (p *modelProvider) fetchCatalogPages(ctx context.Context, cfg providerConfig, rawURL string) ([]*catalogModel, error) {
	pg, client, err := p.fetchFirstCatalogPage(ctx, cfg, rawURL)
	if err != nil {
		return nil, err
	}
	listed := append([]*catalogModel(nil), pg.models...)
	for page := 1; page < providerCatalogMaxPages && pg.nextPageToken != ""; page++ {
		pg, err = p.fetchCatalogPage(ctx, cfg, rawURL, pg.nextPageToken, client)
		if err != nil {
			return nil, err
		}
		listed = append(listed, pg.models...)
	}
	return listed, nil
}

// fetchFirstCatalogPage 拉取目录第一页, 出口策略(按序):
//  1. 节点出口, 网络错误/出口地区被拒时冷却该节点换下一个 ——
//     节点池里每个健康节点各试一次, 不设固定次数上限;
//  2. 节点全部失败后, 用直连兜底再试最后一次。
//
// 直连模式本身就只有一步, 不会轮换也不会兜底。返回胜出的客户端供翻页沿用。
func (p *modelProvider) fetchFirstCatalogPage(ctx context.Context, cfg providerConfig, rawURL string) (catalogPage, *http.Client, error) {
	pg, err := p.fetchCatalogPage(ctx, cfg, rawURL, "", providerExitClient())
	if err == nil {
		return pg, providerExitClient(), nil
	}
	if exitModeDirectNow() {
		return catalogPage{}, nil, err
	}
	for p.retryCatalogOnNextExit(err) {
		pg, err = p.fetchCatalogPage(ctx, cfg, rawURL, "", providerExitClient())
		if err == nil {
			return pg, providerExitClient(), nil
		}
	}
	// 直连兜底: 只在真的走过节点出口后才有意义 —— 池里没有出口时,
	// 出口客户端本身就是直连, 再试一次纯属浪费。
	// 兜底尝试: 同样走网关出口(跟随模式) —— 代理模式下若确实无可用节点,
	// 由出口决策按"节点全挂兜底"开关决定是否直连。
	if !p.catalogDirectTried && len(effectiveProxyList()) > 0 {
		p.catalogDirectTried = true
		log.Printf("  providers: %s 目录经节点全部失败, 走出口兜底再试一次", p.name)
		cl := catalogFallbackClient()
		pg, derr := p.fetchCatalogPage(ctx, cfg, rawURL, "", cl)
		if derr == nil {
			return pg, cl, nil
		}
		log.Printf("  providers: %s 直连兜底也失败: %v", p.name, derr)
	}
	return catalogPage{}, nil, err
}

// catalogExitBudget 目录轮换预算 = 节点池里**健康**的出口数。
//
// 只数健康节点: 连通检测已判定不可达的节点, 每个都要白等一次超时才轮到下一个,
// 订阅池几十上百个节点时会把一次目录刷新拖满 providerCatalogTimeout, 期间
// 管理面板与业务请求全被拖慢 —— 这正是"页面加载失败 / 切页很卡"的成因之一。
//
// 冷却会逐步把失败出口移出可用集合, 所以这个预算本身就是递减的, 轮换必然收敛。
func catalogExitBudget() int {
	list := effectiveProxyList()
	if len(list) == 0 {
		return 1
	}
	healthy, unknown := 0, 0
	for i, p := range list {
		if !zenProxyAvailable(i) || !nodeDialable(p) {
			continue // 冷却中 / 本地入站没起来: 试了也是白等一次超时
		}
		switch healthOf(nodeLocalKey(p)) {
		case "ok":
			healthy++
		case "fail":
			// 连通检测明确判定不可达: 不占预算
		default:
			unknown++ // 还没探测过: 按"未探测即可用"处理, 不能当不可用
		}
	}
	n := healthy
	if n == 0 {
		n = unknown
	}
	if n == 0 {
		// 池里出口全在冷却或全部不可达: 别再空转, 让直连兜底尽快接手
		return 1
	}
	if n > catalogExitMaxRotations {
		return catalogExitMaxRotations
	}
	return n
}

// retryCatalogOnNextExit 目录抓取的换出口判定。网络错误或出口地区被拒时,
// 冷却当前出口并返回 true 让调用方换下一个重试; 池里健康节点试完返回 false。
// 普通 4xx/5xx 换节点无意义, 原样返回真实错误。
func (p *modelProvider) retryCatalogOnNextExit(err error) bool {
	if p.catalogExitRetries >= catalogExitBudget() {
		return false
	}
	// 总时长上限: 预算再大也不让一次目录刷新长时间霸占出口
	if !p.catalogExitAt.IsZero() && time.Since(p.catalogExitAt) > catalogExitRotateMax {
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
	if p.catalogExitRetries == 0 {
		p.catalogExitAt = time.Now()
	}
	p.catalogExitRetries++
	reason := fmt.Sprintf("network error (%v)", err)
	if regionRejected {
		reason = fmt.Sprintf("exit region rejected: %s", kit.Truncate(string(body), 160))
	}
	rotateProviderExit(p.name, "catalog "+reason, p.catalogExitRetries, catalogExitBudget())
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
//
// 同一个 provider 同时只允许一次刷新在跑: 定时循环、面板手动刷新、请求路径
// 的按需刷新、discovery 都可能并发触发, 而每次刷新都可能长时间轮换出口。
// 重复触发时后到者直接返回(先到者会把结果写回), 否则出口池会被同一份目录
// 的多个副本同时打爆 —— 这正是面板卡顿与"加载失败"的来源之一。
func (p *modelProvider) refreshCatalog(ctx context.Context, force bool) error {
	cfg, _ := providerConfigFor(p.name)
	if !cfg.Catalog {
		return nil
	}
	p.mu.Lock()
	if p.catalogInflight {
		p.mu.Unlock()
		return nil
	}
	now := time.Now().UnixMilli()
	if !force && now-p.attemptedAt < int64(providerCatalogRefresh/time.Millisecond) &&
		(len(p.catalog) > 0 || p.catalogErr != "") {
		p.mu.Unlock()
		return nil
	}
	p.catalogInflight = true
	p.attemptedAt = now
	p.catalogExitRetries = 0
	p.catalogExitAt = time.Time{}
	p.catalogDirectTried = false
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.catalogInflight = false
		p.mu.Unlock()
	}()

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

	if explicit, ok := cfg.explicitModels(); ok {
		var candidates []catalogModel
		for id, enabled := range explicit {
			if enabled {
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
		if !catalogHasPrices(cat) {
			// 无价格目录: 按白名单出候选(与 eval 侧回退一致, 否则恒为空)。
			for id := range cfg.freeSet() {
				candidates = append(candidates, catalogModel{ID: id})
			}
			break
		}
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

// catalogModels 全量目录(含被勾掉的), 供面板勾选列表用。
// 只收录聊天模型; 排序稳定; disabled 来自配置的 DisabledModels。
func (p *modelProvider) catalogModels() []map[string]any {
	cfg, _ := providerConfigFor(p.name)
	disabled := cfg.disabledSet()
	p.mu.Lock()
	cat := p.catalog
	p.mu.Unlock()
	out := make([]map[string]any, 0, len(cat))
	for _, m := range cat {
		if !isChatModel(m) {
			continue
		}
		out = append(out, map[string]any{"id": m.ID, "disabled": disabled[m.ID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
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
		// 启动后先让出资源: 订阅解析、节点实例重建、连通检测都在同一时刻发生,
		// 若目录刷新同时开跑, 出口池会被三类任务一起抢, 面板刚打开就是"加载失败"。
		time.Sleep(providerCatalogStartupDelay)
		refreshOnce()
		t := time.NewTicker(providerCatalogRefresh)
		defer t.Stop()
		for range t.C {
			refreshOnce()
		}
	}()
}
