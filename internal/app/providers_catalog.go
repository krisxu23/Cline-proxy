package app

import (
	"context"
	"encoding/json"
	"errors"
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

// isFree 该模型在本 provider 上是否可用 —— 只认显式开关:
// Models 为空(尚未迁移)一律不可用; 缺 key 与永久拒绝直接否决。
func (p *modelProvider) isFree(modelID string) bool {
	cfg, _ := providerConfigFor(p.name)
	if len(enabledAPIKeys(cfg, p.name)) == 0 {
		return false
	}
	p.mu.Lock()
	rejected := p.rejected
	p.mu.Unlock()
	if _, bad := rejected[modelID]; bad {
		return false
	}
	if explicit, ok := cfg.explicitModels(); ok {
		return explicit[modelID]
	}
	return false
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
	//
	// 从 16 降到 4: 轮换本身由 pickZenProxyWhere 的全局 round-robin 计数驱动,
	// 每次拨号天然换一个出口, 不需要靠"多试几次"来碰运气。而 16 的代价实测是
	// 灾难性的 —— 健康出口只有 25~95 个时, 9 个 provider 各轮换 16 次 = 144 次
	// 出口冷却, 直接把池子抽干(2026-09-17 实证: 池空 → 全量走直连 → 全部失败)。
	catalogExitMaxRotations = 4
	// catalogFailBackoffMax 连续失败后退避上限。超过半天还没拉到的 provider
	// 继续高频重试没有收益(目录只用于展示与勾选, 不阻塞对话)。
	catalogFailBackoffMax = 6 * time.Hour
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
	if isGoogleProvider(cfg) {
		return googleOpenAIBase(cfg.BaseURL) + "/models"
	}
	return strings.TrimRight(cfg.BaseURL, "/") + "/models"
}

// catalogHeaders 目录请求的鉴权头: 与对话请求同一套规则,
// Google 按目标路径择一(原生目录只认 x-goog-api-key, 见 googleUsesBearer)。
func catalogHeaders(cfg providerConfig, target string) map[string]string {
	if !hasEnabledKey(cfg) {
		return nil
	}
	out := map[string]string{}
	cfg.applyAuth(target, func(k, v string) { out[k] = v })
	return out
}

// nativeCatalogParams 该目录地址是否使用 Google 原生分页参数。
// 只有已知的 Google 原生目录(isGoogleProvider 且路径不含 /openai)才带 pageSize;
// 其余端点(普通 /v1/models、严格签名校验的自建网关)加未知查询参数可能被 400 拒掉。
func nativeCatalogParams(cfg providerConfig, rawURL string) bool {
	return isGoogleProvider(cfg) && !strings.Contains(strings.ToLower(rawURL), "/openai")
}

func (p *modelProvider) fetchCatalogPage(ctx context.Context, cfg providerConfig, rawURL, pageToken string, client *http.Client) (catalogPage, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return catalogPage{}, err
	}
	q := u.Query()
	if nativeCatalogParams(cfg, rawURL) {
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

// catalogFallbackClient 目录抓取的**直连**兜底客户端(不经出口池)。
//
// 此前它返回的是同一个出口客户端, 所谓"兜底"等于再撞一次同样的死节点 ——
// 日志实证: `直连兜底也失败: socks5: general SOCKS server failure`。
// 目录抓取是控制面请求: 目标站点通常直连就通(实测 opencode.ai 直连 1.3s 200),
// 不该被数据面出口池的整体状况拖死。显式关闭 rescueDirect 时返回 nil, 调用方跳过兜底。
//
// (此处原有第二段与本节矛盾的旧注释 —— "走网关统一出口, 代理模式经节点…现已收回
//
//	统一决策"。代码从来是直连(directHTTPClient), 且"收回统一决策"会让控制面被数据面
//	的整体故障拖死。2026-09-18 复核后删除, 以本段为准。)
func catalogFallbackClient() *http.Client {
	if !rescueDirectEnabled() {
		return nil
	}
	return directHTTPClient()
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
		cl := catalogFallbackClient()
		if cl == nil {
			log.Printf("  providers: %s 目录经节点全部失败(直连兜底已关闭)", p.name)
			return catalogPage{}, nil, err
		}
		log.Printf("  providers: %s 目录经节点全部失败, 走直连兜底再试一次", p.name)
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
	for _, p := range list {
		if !zenProxyAvailable(p) || !nodeDialable(p) {
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
	if status == 0 && !isCatalogRotatableErr(err) {
		return false // 调用方取消/超时、URL 解析、响应形状: 换哪个出口都会复现
	}
	if p.catalogExitRetries == 0 {
		p.catalogExitAt = time.Now()
	}
	p.catalogExitRetries++
	reason := fmt.Sprintf("network error (%v)", err)
	if regionRejected {
		reason = fmt.Sprintf("exit region rejected: %s", kit.Truncate(string(body), 160))
	}
	// ★ 控制面失败**不冷却数据面出口池**。
	//
	// 旧实现调 rotateProviderExit → cooldownZenProxyByIndex(2 分钟)。语义是错的:
	// 一个节点可能拉不到某上游的 /models(控制面), 但对话(数据面)完全正常;
	// 而且聚合冷却量无上界 —— 9 个 provider × 每次 16 轮换 = 144 次冷却, 而
	// 实测健康出口只有 25~95 个, 于是池子被抽干, 所有请求退化成直连并失败
	// (2026-09-17 实证: 日志末尾满屏"节点池无可用出口")。
	//
	// 轮换不依赖冷却: pickZenProxyWhere 的全局 round-robin 计数每次拨号都 +1,
	// 下一次尝试天然落在另一个出口上。数据面失败仍照常冷却(providers_chat.go),
	// 那才是冷却该发生的地方。
	log.Printf("  providers: %s catalog %s, retry %d/%d on the next exit (控制面失败不计入出口冷却)",
		p.name, reason, p.catalogExitRetries, catalogExitBudget())
	return true
}

// catalogRefreshInterval 该 provider 当前的目录刷新间隔(指数退避)。
//
// 连续失败会放大间隔: 15min → 30min → 1h → 2h → 4h → 6h(封顶), 成功清零。
// 动机(2026-09-17 实证): 目录刷新失败时每次都会轮换出口, 而"请求路径按需刷新"
// (providers_chat.go) 在目录为空时每分钟就会触发一次 —— 一个永久拉不到目录的
// provider 因此每分钟烧掉一轮出口轮换, 把只有几十个健康出口的池子抽干, 最终
// 所有请求无出口可用、全部走直连而失败。退避把这种空转压到可忽略。
func (p *modelProvider) catalogRefreshInterval() time.Duration {
	p.mu.Lock()
	streak := p.catalogFailStreak
	p.mu.Unlock()
	return catalogIntervalFor(streak)
}

// catalogIntervalFor 纯函数版退避计算(不取锁), 供已持 p.mu 的调用点内联使用。
//
// 基准 15min; 首次失败仍维持基准(单次抖动应能很快恢复), 从第 2 次连续失败起
// 逐步翻倍, 封顶 6h:
//
//	streak  0/1 → 15min
//	        2   → 30min
//	        3   → 1h
//	        4   → 2h
//	        5   → 4h
//	        6+  → 6h(封顶)
func catalogIntervalFor(streak int) time.Duration {
	iv := providerCatalogRefresh
	for i := 1; i < streak && iv < catalogFailBackoffMax; i++ {
		iv *= 2
	}
	if iv > catalogFailBackoffMax {
		iv = catalogFailBackoffMax
	}
	return iv
}

// catalogFastRetryStreak 请求路径保持"1 分钟快速重试"的连续失败次数上限。
// 达到该次数后, 请求路径改用 catalogIntervalFor 的退避门限:
// 暂态抖动要能快速恢复, 而**持续拉不到目录**的 provider 不能无限期每分钟空转。
const catalogFastRetryStreak = 3

// catalogRequestGateMs 请求路径的目录刷新门限(毫秒)。
//
//	连续失败 < catalogFastRetryStreak → 1 分钟(目录空着要尽快补上)
//	否则                            → 退避间隔(15min→…→6h)
//
// 这条分级是必需的: 只用 1 分钟会让永久失败的 provider 被每个请求触发刷新,
// 每分钟烧掉一轮出口轮换; 只用退避又会让单次上游抖动锁死目录几十分钟。
func catalogRequestGateMs(streak int) int64 {
	if streak < catalogFastRetryStreak {
		return providerCatalogRetryMs
	}
	return int64(catalogIntervalFor(streak) / time.Millisecond)
}

// noteCatalogFailure 记录一次目录刷新失败(放大退避间隔)。
func (p *modelProvider) noteCatalogFailure() {
	p.mu.Lock()
	p.catalogFailStreak++
	p.mu.Unlock()
}

// noteCatalogSuccess 目录刷新成功: 清零退避, 恢复基准间隔。
func (p *modelProvider) noteCatalogSuccess() {
	p.mu.Lock()
	p.catalogFailStreak = 0
	p.mu.Unlock()
}

// isCatalogRotatableErr status==0 的错误里只有真网络问题才值得换出口重试:
// 调用方取消/超时、URL 解析失败、响应形状不可识别换个出口照样复现。
func isCatalogRotatableErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errUnrecognisedCatalogShape) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Op == "parse" {
		return false
	}
	return true
}

// errUnrecognisedCatalogShape 200 但响应形状不可识别: 换出口重打必然复现,
// 不属于可轮换的网络错误(见 retryCatalogOnNextExit)。
var errUnrecognisedCatalogShape = errors.New("unrecognised catalog response shape")

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
// 的按需刷新都可能并发触发, 而每次刷新都可能长时间轮换出口。
// 重复触发时后到者直接返回(先到者会把结果写回), 否则出口池会被同一份目录
// 的多个副本同时打爆 —— 这正是面板卡顿与"加载失败"的来源之一。
func (p *modelProvider) refreshCatalog(ctx context.Context, force bool) error {
	cfg, _ := providerConfigFor(p.name)
	if !cfg.Catalog {
		// 无目录 provider 没有可拉取的列表, 但 legacy 白名单仍要迁移:
		// 在这里折叠为显式开关, 之后每次调用都是无操作。
		p.maybeBackfillExplicitModels()
		return nil
	}
	p.mu.Lock()
	if p.catalogInflight {
		p.mu.Unlock()
		return nil
	}
	now := time.Now().UnixMilli()
	// 间隔用**退避后**的值: 连续失败的 provider 逐步拉长到 6h, 不再每分钟空转。
	minInterval := int64(catalogIntervalFor(p.catalogFailStreak) / time.Millisecond)
	if !force && now-p.attemptedAt < minInterval &&
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
		// 调用方主动取消(客户端断开/面板中止)不算连续失败: 它没有证明目录
		// 拉不下来, 却会把退避逐级放大到 6h, 期间请求路径全部失败。
		// 超时(deadline)是真实失败, 照常计数。
		if !errors.Is(lastErr, context.Canceled) {
			p.noteCatalogFailure()
		}
		p.mu.Lock()
		p.catalogErr = lastErr.Error()
		p.mu.Unlock()
		return lastErr
	}
	if len(listed) == 0 {
		err := fmt.Errorf("catalog response listed no models")
		p.noteCatalogFailure()
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
	// 成功: 清零退避, 下次恢复基准 15 分钟间隔。
	p.noteCatalogSuccess()
	p.maybeBackfillExplicitModels()
	return nil
}

// maybeBackfillExplicitModels 把 legacy 配置一次性快照为显式模型开关。
// 已迁移 / 已有显式条目(面板写过就不再覆盖) / 快照为空时跳过。
//
// 快照取并集(宁多勿少): 白名单里的手填模型固化为启用 + 目录里的聊天模型
// 固化为禁用(待用户在面板勾选 opt-in), 被勾掉的折叠为 Enabled=false ——
// 目录模型默认不直接可服务, 避免 ex-pricing 用户刷新后付费模型自动上线。
// 永久拒绝的不固化(判定层仍会拦, 但不占用开关位)。
func (p *modelProvider) maybeBackfillExplicitModels() {
	cfg, ok := providerConfigFor(p.name)
	if !ok || cfg.Migrated || len(cfg.Models) > 0 {
		return
	}
	p.mu.Lock()
	cat := p.catalog
	rejected := p.rejected
	p.mu.Unlock()
	disabledIDs := cfg.disabledSet()
	entries := make([]providerModelEntry, 0, len(cat)+len(cfg.FreeModels)+len(disabledIDs))
	seen := make(map[string]bool, len(cat)+len(cfg.FreeModels)+len(disabledIDs))
	add := func(id string, enabled bool) {
		if id = strings.TrimSpace(id); id == "" || seen[id] {
			return
		}
		if enabled {
			if _, bad := rejected[id]; bad {
				return
			}
			if disabledIDs[id] {
				return // 被勾掉的不进启用分支, 后面统一折叠为禁用
			}
		}
		seen[id] = true
		entries = append(entries, providerModelEntry{ID: id, Enabled: enabled})
	}
	for _, id := range cfg.FreeModels {
		add(id, true)
	}
	for _, m := range cat {
		if isChatModel(m) {
			add(m.ID, false)
		}
	}
	if len(entries) == 0 && len(disabledIDs) == 0 {
		return
	}
	for id := range disabledIDs {
		if seen[id] {
			continue
		}
		add(id, false)
	}
	if len(entries) == 0 {
		return
	}
	name := p.name
	mutateProvidersConfig(func(cfg *zenConfigData) {
		pc := cfg.Providers[name]
		pc.Models = entries
		pc.Migrated = true
		cfg.Providers[name] = pc
	})
}

// freeModelIDs 该 provider 的已启用模型列表(只认显式开关)。
// 对外发布(providerModelList)与 isFree 同源:
// 缺 key、未迁移、已永久剔除的模型不得出现。
func (p *modelProvider) freeModelIDs() []catalogModel {
	cfg, _ := providerConfigFor(p.name)
	if len(enabledAPIKeys(cfg, p.name)) == 0 {
		return nil
	}
	explicit, ok := cfg.explicitModels()
	if !ok {
		return nil
	}
	p.mu.Lock()
	rejected := p.rejected
	p.mu.Unlock()
	out := make([]catalogModel, 0, len(explicit))
	for id, enabled := range explicit {
		if !enabled {
			continue
		}
		if _, bad := rejected[id]; bad {
			continue
		}
		out = append(out, catalogModel{ID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// catalogModels 全量目录(含被勾掉的), 供面板勾选列表用。
// 只收录聊天模型; 排序稳定; 启用状态来自显式开关(未迁移时默认全启用)。
func (p *modelProvider) catalogModels() []map[string]any {
	cfg, _ := providerConfigFor(p.name)
	explicit, _ := cfg.explicitModels()
	p.mu.Lock()
	cat := p.catalog
	p.mu.Unlock()
	out := make([]map[string]any, 0, len(cat))
	for _, m := range cat {
		if !isChatModel(m) {
			continue
		}
		out = append(out, map[string]any{"id": m.ID, "disabled": explicit != nil && !explicit[m.ID]})
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
		if len(enabledAPIKeys(cfg, name)) == 0 {
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
	explicit, _ := cfg.explicitModels()
	freeCount, chatCount := 0, 0
	for id, enabled := range explicit {
		if !enabled {
			continue
		}
		if _, bad := p.rejected[id]; !bad {
			freeCount++
		}
	}
	for _, m := range p.catalog {
		if isChatModel(m) {
			chatCount++
		}
	}
	return map[string]any{
		"configured":  len(enabledAPIKeys(cfg, p.name)) > 0,
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
				if p == nil || !ok {
					continue
				}
				if !pc.Catalog {
					// 无目录 provider 不拉取, 但要把 legacy 白名单迁移为显式开关。
					p.maybeBackfillExplicitModels()
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
		for {
			select {
			case <-t.C:
				refreshOnce()
			case <-appRootCtx.Done():
				// 收到退出信号: 停止 provider 目录刷新协程, 让进程能够真正停下。
				return
			}
		}
	}()
}
