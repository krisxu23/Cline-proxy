package app

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// providerHeaderSpec 请求头规格: 值可来自环境变量, 或固定默认值; "${origin}" 运行时展开。
type providerHeaderSpec struct {
	Env     string `json:"env,omitempty"`
	Default string `json:"default,omitempty"`
}

// providerAPIKey 单个 API Key 的显式开关。
type providerAPIKey struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// providerModelEntry 单个模型的显式开关。
type providerModelEntry struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// providerConfig 一个通用 OpenAI 兼容上游的配置。
//
// 面板只要求 Provider 名 + Base URL + API Key 三项即可自动拉取模型目录;
// 其余字段都有默认值, 且 Google 的路径/鉴权方言由代码推导, 不需要用户填。
type providerConfig struct {
	BaseURL string                        `json:"baseUrl"`
	APIKey  string                        `json:"apiKey"`
	Catalog bool                          `json:"catalog"` // 拉取 /models 目录
	Headers map[string]providerHeaderSpec `json:"headers,omitempty"`
	// FreeModels / DisabledModels 已废弃: 只作为一次性迁移的输入
	// (maybeBackfillExplicitModels), 请求路径不再查它们。
	// Migrated 置位后即为摆设, 保留字段只是为了不丢弃尚未迁移的配置。
	FreeModels     []string `json:"freeModels,omitempty"`
	DisabledModels []string `json:"disabledModels,omitempty"`
	// DisplayName 面板展示名; APIType 上游方言("openai" 默认, "anthropic" 可选);
	// Enabled 总开关(nil = true); APIKeys 显式 key 列表; Models 显式模型开关;
	// Migrated 是否已从 legacy 字段回填。
	DisplayName string               `json:"name,omitempty"`
	APIType     string               `json:"apiType,omitempty"`
	Enabled     *bool                `json:"enabled,omitempty"`
	APIKeys     []providerAPIKey     `json:"apiKeys,omitempty"`
	Models      []providerModelEntry `json:"models,omitempty"`
	Migrated    bool                 `json:"migrated,omitempty"`
}

var providerIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func normalizeModelID(id string) (string, string) {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "zen/") {
		return "opencode", strings.TrimPrefix(id, "zen/")
	}
	if strings.HasPrefix(id, "cline-pass/") {
		return "clinepass", strings.TrimPrefix(id, "cline-pass/")
	}
	if i := strings.Index(id, ":"); i > 0 && !strings.Contains(id[:i], "/") {
		return id[:i], id[i+1:]
	}
	if i := strings.Index(id, "/"); i > 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}
func isBuiltinProvider(id string) bool { return id == "opencode" || id == "cline" || id == "clinepass" }

// ============ Google Gemini 特例(写死在代码里) ============
//
// Google AI Studio 只需要填 https://generativelanguage.googleapis.com 与 key,
// 其余约定由这里统一补齐, 面板上不再暴露 Models URL / Models Key 头:
//   - 端点: 一律归一到 OpenAI 兼容前缀 /v1beta/openai(官方同时提供原生方言,
//     但网关只需要一种, 免去用户自己拼路径出错 —— 例如 /v1beta/model 少个 s)。
//   - 鉴权: OpenAI 兼容路径认 Authorization: Bearer, 原生路径认 x-goog-api-key,
//     两种同时发送, 因此两种形态的 key 都能直接用。
//   - 目录: /models 返回 OpenAI 形状(id 形如 models/gemini-3.8-flash), 发布时
//     去掉 models/ 前缀, 上游收到的模型名与用户输入一致。

const (
	googleAPIHost    = "generativelanguage.googleapis.com"
	googleOpenAIPath = "/v1beta/openai"
	googleKeyHeader  = "x-goog-api-key"
)

// isGoogleProvider Base URL 是否指向 Google Gemini。
//
// 判据取**主机名**, 而不是整条 URL 的子串包含: 后者会让任何在路径或查询参数里
// 出现该域名的第三方端点(例如自建反代 https://my-proxy/gemini?upstream=
// generativelanguage.googleapis.com)被误判成 Google, 进而被强制打开目录抓取、
// 切到 Google 鉴权方言、并被剥掉路径前缀。
func isGoogleProvider(c providerConfig) bool {
	base := strings.TrimSpace(c.BaseURL)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return strings.Contains(strings.ToLower(base), googleAPIHost)
	}
	host := strings.ToLower(u.Hostname())
	return host == googleAPIHost || strings.HasSuffix(host, "."+googleAPIHost)
}

// googleOpenAIBase 把任意 Gemini Base URL 归一到 OpenAI 兼容前缀。
//
// Google 的 OpenAI 兼容方言固定挂在 /v1beta/openai 下, 所以不管用户填的是裸域名、
// /v1beta、/v1beta/models 还是 /v1, 都应该落到同一个前缀 —— 面板只要求填
// API 地址 + Key, 路径形态由代码推导。
//
// 此前只有 /v1beta 系列与空路径会走归一化, 其余走的是 `base + "/v1beta/openai"`
// 这条兜底, 于是 https://generativelanguage.googleapis.com/v1/models 会被拼成
// .../v1/models/v1beta/openai 这种畸形地址。
func googleOpenAIBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base + googleOpenAIPath
	}
	// 用户已经自己填了兼容前缀, 原样尊重, 不再叠加。
	if strings.Contains(strings.ToLower(u.Path), "/openai") {
		return base
	}
	// 其余一律收敛到 scheme://host/v1beta/openai。能走到这里的主体必然是
	// generativelanguage.googleapis.com(见 isGoogleProvider), 丢弃 path 是安全的。
	return u.Scheme + "://" + u.Host + googleOpenAIPath
}

// googleNativeCatalogURL Google 原生目录地址(带 x-goog-api-key 与 pageToken 分页)。
func googleNativeCatalogURL(c providerConfig) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"))
	if err != nil || u.Host == "" {
		return strings.TrimRight(c.BaseURL, "/") + "/v1beta/models"
	}
	return u.Scheme + "://" + u.Host + "/v1beta/models"
}

// chatEndpoint 本次请求要打到上游的完整地址。
func (c providerConfig) chatEndpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if isGoogleProvider(c) {
		base = googleOpenAIBase(base)
	}
	return base + "/chat/completions"
}

// googleUsesBearer 只有 Google 的 OpenAI 兼容方言认 Bearer。
//
// 原生方言(/v1beta/models)收到 Bearer 会直接回 401
// "API keys are not supported by this API. Expected OAuth2 access token ...",
// 把真正的失败原因整个盖掉 —— 实测在不受支持的地区访问时,
// 真实响应是 400 "User location is not supported for the API use",
// 但多发一个 Bearer 就只剩 401, 让人误以为是 key 的问题。
func googleUsesBearer(target string) bool {
	return strings.Contains(strings.ToLower(target), "/openai")
}

// applyAuth 写入该 provider 需要的鉴权头。
// target 是本次请求的完整地址: 同一个 Google key 在两种方言下鉴权方式不同,
// 必须按目标路径择一, 否则原生路径上的 Bearer 会被拒并掩盖真实错误。
func (c providerConfig) applyAuth(target string, set func(key, value string)) {
	c.applyAuthWithKey(target, c.APIKey, set)
}

// applyAuthWithKey 按单 key 写入鉴权头(Chat 多 key 轮换逐 key 调用)。
// APIType "anthropic" 走 x-api-key + 版本头, 否则沿用 Google 方言/Bearer。
func (c providerConfig) applyAuthWithKey(target, key string, set func(key, value string)) {
	if key == "" {
		return
	}
	if c.APIType == "anthropic" {
		set("x-api-key", key)
		set("anthropic-version", "2023-06-01")
		return
	}
	if isGoogleProvider(c) {
		set(googleKeyHeader, key)
		if googleUsesBearer(target) {
			set("Authorization", "Bearer "+key)
		}
		return
	}
	set("Authorization", "Bearer "+key)
}

// hasEnabledKey 是否有可用的 key(不看冷却, 只看显式开关; 无 provider 名时用)。
func hasEnabledKey(cfg providerConfig) bool {
	if !cfg.isEnabled() {
		return false
	}
	for _, e := range cfg.apiKeys() {
		if e.Enabled && strings.TrimSpace(e.Key) != "" {
			return true
		}
	}
	return false
}

// enabledAPIKeys 可轮换的 key 列表: 健康打散在前、被冷却的沉底。
// 只看显式开关, 不依赖 legacy 单 key 是否回填 —— 只有 APIKeys 列表的
// provider 同样视为已配置。
func enabledAPIKeys(cfg providerConfig, provider string) []string {
	if !cfg.isEnabled() {
		return nil
	}
	var head, tail []string
	for _, e := range cfg.apiKeys() {
		if !e.Enabled || strings.TrimSpace(e.Key) == "" {
			continue
		}
		if isKeyDemoted(provider, e.Key) {
			tail = append(tail, e.Key)
		} else {
			head = append(head, e.Key)
		}
	}
	rand.Shuffle(len(head), func(i, j int) { head[i], head[j] = head[j], head[i] })
	return append(head, tail...)
}

// stripProviderModelPrefix Google 目录里的 id 带 models/ 前缀, 发布时去掉。
func (c providerConfig) stripProviderModelPrefix(id string) string {
	if !isGoogleProvider(c) {
		return id
	}
	return strings.TrimPrefix(id, "models/")
}

// isEnabled 总开关(nil = true)。
func (c providerConfig) isEnabled() bool { return c.Enabled == nil || *c.Enabled }

// apiKeys 显式 key 列表; 为空时从 legacy 单 APIKey 回填 [{key,true}]。
func (c providerConfig) apiKeys() []providerAPIKey {
	if len(c.APIKeys) > 0 {
		return c.APIKeys
	}
	if c.APIKey != "" {
		return []providerAPIKey{{Key: c.APIKey, Enabled: true}}
	}
	return nil
}

// explicitModels 显式模型开关表; Models 为空时返回 (nil,false) 表示尚未迁移,
// 调用方视为无可用模型(等 backfill 或面板写入显式条目)。
func (c providerConfig) explicitModels() (map[string]bool, bool) {
	if len(c.Models) == 0 {
		return nil, false
	}
	m := make(map[string]bool, len(c.Models))
	for _, e := range c.Models {
		if id := strings.TrimSpace(e.ID); id != "" {
			m[id] = e.Enabled
		}
	}
	return m, true
}

// freeSet legacy 白名单集合 —— 仅一次性迁移的输入, 请求路径不再使用。
func (c providerConfig) freeSet() map[string]bool {
	s := make(map[string]bool, len(c.FreeModels))
	for _, m := range c.FreeModels {
		if m = strings.TrimSpace(m); m != "" {
			s[m] = true
		}
	}
	return s
}

// disabledSet legacy 剔除集合 —— 仅一次性迁移的输入, 请求路径不再使用。
func (c providerConfig) disabledSet() map[string]bool {
	s := make(map[string]bool, len(c.DisabledModels))
	for _, m := range c.DisabledModels {
		if m = strings.TrimSpace(m); m != "" {
			s[m] = true
		}
	}
	return s
}

// resolveHeader 解析请求头; ${origin} 展开为本地网关地址。
func (c providerConfig) resolveHeader(spec providerHeaderSpec, origin string) string {
	if spec.Env != "" {
		if v := os.Getenv(spec.Env); v != "" {
			return strings.ReplaceAll(v, "${origin}", origin)
		}
	}
	return strings.ReplaceAll(spec.Default, "${origin}", origin)
}

// ============ 目录与签名类型 ============

// catalogModel 归一化后的模型目录条目。
type catalogModel struct {
	ID               string
	Name             string
	Tokenizer        string
	ChatCapable      *bool // 目录明确声明则优先
	OutputModalities []string
	ContextLength    int
	MaxOutput        int
	Created          int64
	PricesKnown      bool
	PromptPrice      float64
	CompletionPrice  float64
}

// thoughtSignatureCache 工具调用签名缓存(LRU)。
type thoughtSignatureCache struct {
	mu    sync.Mutex
	m     map[string]string
	order []string
	max   int
}

func newThoughtSignatureCache(max int) *thoughtSignatureCache {
	if max <= 0 {
		max = 5000
	}
	return &thoughtSignatureCache{m: map[string]string{}, max: max}
}

// ============ 运行时注册表 ============

var (
	providerRTMu sync.Mutex
	providerRT   = map[string]*modelProvider{}
)

// modelProvider 单个 provider 的运行时状态。
type modelProvider struct {
	name string

	mu                 sync.Mutex
	catalog            map[string]*catalogModel
	slugs              map[string]*catalogModel
	fetchedAt          int64 // unix ms
	attemptedAt        int64
	catalogErr         string
	catalogExitRetries int               // 本次目录刷新已用的换出口次数(预算 = 池内健康出口数)
	catalogExitAt      time.Time         // 本次目录刷新开始轮换的时刻(用于总时长上限)
	catalogDirectTried bool              // 本次目录刷新是否已用过直连兜底
	catalogInflight    bool              // 是否已有一次目录刷新在跑(防并发重复刷新)
	rejected           map[string]string // modelID -> 永久拒绝原因
	sigCache           *thoughtSignatureCache
}

func newModelProvider(name string) *modelProvider {
	return &modelProvider{
		name:     name,
		rejected: map[string]string{},
		sigCache: newThoughtSignatureCache(5000),
	}
}

// providerConfigFor 锁内读取单个 provider 配置; ok=false 表示未声明。
// 必须经此入口读取: Providers 是 map, 与 mutateProvidersConfig 的写入并发时
// 直接读取会触发 Go 不可恢复的 "concurrent map read and map write"。
func providerConfigFor(name string) (providerConfig, bool) {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	if zenConfig == nil {
		return providerConfig{}, false
	}
	pc, ok := zenConfig.Providers[name]
	return pc, ok
}

// providerByName 按名取运行时状态; 配置中不存在则返回 nil。
func providerByName(name string) *modelProvider {
	if _, ok := providerConfigFor(name); !ok {
		return nil
	}
	providerRTMu.Lock()
	defer providerRTMu.Unlock()
	p, ok := providerRT[name]
	if !ok {
		p = newModelProvider(name)
		providerRT[name] = p
	}
	return p
}

// providerNames 配置中已声明的 provider 名(排序)。
func providerNames() []string {
	zenConfigMu.Lock()
	var names []string
	if zenConfig != nil {
		names = make([]string, 0, len(zenConfig.Providers))
		for n := range zenConfig.Providers {
			names = append(names, n)
		}
	}
	zenConfigMu.Unlock()
	sort.Strings(names)
	return names
}

// normalizeProviderConfig 落地前的归一化。目前只有 Google 需要:
// 目录默认开启(用户只填地址和 key 就该自动拉到模型列表)。
func normalizeProviderConfig(c providerConfig) providerConfig {
	if !isGoogleProvider(c) {
		return c
	}
	c.Catalog = true
	return c
}

// validateProviderConfig 校验 provider 配置。
func validateProviderConfig(name string, c providerConfig) error {
	if !providerIDRe.MatchString(name) {
		return fmt.Errorf("provider id %q must match %s", name, providerIDRe)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("provider %s: baseUrl is required", name)
	}
	// baseUrl 会由服务端直接请求(目录抓取与对话转发), 属于 SSRF 面。
	// 此前只校验了非空, 于是 file:// 或云元数据地址都能存进来。
	// 现在要求显式 http/https, 并拒绝链路本地地址。
	if err := validateOutboundURL(c.BaseURL); err != nil {
		return fmt.Errorf("provider %s: %w", name, err)
	}
	return nil
}

// mergeProviderConfigPatch 把请求体里**实际出现**的字段覆盖到底配置上。
//
// providers/update 原本是整体替换: 直接调 API 只传 {"baseUrl": "..."} 就会把
// apiKeys / models / migrated 一并清空; 其中 migrated 变回 false 且 models 为空,
// 会让下一次目录刷新走 backfill 分支, 把用户启用过的模型集体重建成"默认关闭"。
// 面板自己会回传整份配置, 所以从界面上看不出问题, 但只要用 API 或脚本改配置
// 就会中招。
//
// 实现刻意走 "struct → map → 覆盖出现的键 → struct", 而不是逐字段 if:
// 逐字段列举正是本轮审查里反复致错的那种写法(新增字段必然被漏掉), 而这里
// 天然与字段集合同步。是否覆盖看的是键**是否出现**, 不是值是否为零 ——
// 否则用户永远没法把一个字段显式清空。
func mergeProviderConfigPatch(base providerConfig, raw map[string]json.RawMessage) (providerConfig, error) {
	if len(raw) == 0 {
		return base, nil
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return base, err
	}
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return base, err
	}
	for k, v := range raw {
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return base, err
	}
	var next providerConfig
	if err := json.Unmarshal(out, &next); err != nil {
		return base, err
	}
	return next, nil
}

// mutateProvidersConfig 是 zen 配置唯一的写入口: 锁内克隆 → 回调改克隆 →
// 整体替换 → 落盘。
//
// 不给调用方裸指针, 也不让回调原地改全局对象, 有两个目的:
//   1. 消除数据竞争。回调内改的是私有克隆, 与热路径上无锁读配置的请求互不干扰。
//   2. 消除"重建配置时漏字段"。每个回调都从完整的当前配置出发, 只会改动它
//      真正关心的字段, 不会把其余字段覆盖成零值。
//
// 注意: 回调在持锁状态下执行, 里面不得再调用 getZenConfig / 其它配置读写函数
// (sync.Mutex 不可重入, 会自锁), 也只应做纯内存改动。
func mutateProvidersConfig(fn func(cfg *zenConfigData)) {
	zenConfigMu.Lock()
	next := zenConfig.clone()
	if next == nil {
		next = defaultZenConfig()
	}
	fn(next)
	zenConfig = next
	zenConfigMu.Unlock()
	saveZenConfig()
}

// ============ 本地网关地址(${origin} 展开) ============

var listenOrigin atomic.Value // string

func setListenOrigin(origin string) {
	listenOrigin.Store(origin)
}

func localOrigin() string {
	if v, ok := listenOrigin.Load().(string); ok && v != "" {
		return v
	}
	return "http://127.0.0.1:3457"
}
