package app

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// providerHeaderSpec 请求头规格: 值可来自环境变量, 或固定默认值; "${origin}" 运行时展开。
type providerHeaderSpec struct {
	Env     string `json:"env,omitempty"`
	Default string `json:"default,omitempty"`
}

// providerConfig 一个通用 OpenAI 兼容上游的配置。
type providerConfig struct {
	BaseURL         string                        `json:"baseUrl"`
	APIKey          string                        `json:"apiKey"`
	Catalog         bool                          `json:"catalog"`       // 拉取 /models 目录
	Pricing         bool                          `json:"pricing"`       // 目录含价格, 按价格判定免费
	ProbeFreeTier   bool                          `json:"probeFreeTier"` // 无价格目录时用最小请求探测(第3期 discovery)
	ChatPath        string                        `json:"chatPath,omitempty"`
	ModelsPath      string                        `json:"modelsPath,omitempty"`
	ModelsURL       string                        `json:"modelsUrl,omitempty"`
	ModelsKeyHeader string                        `json:"modelsKeyHeader,omitempty"`
	Headers         map[string]providerHeaderSpec `json:"headers,omitempty"`
	FreeModels      []string                      `json:"freeModels,omitempty"`
}

var providerIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// chatPath 默认 /chat/completions
func (c providerConfig) chatPath() string {
	if c.ChatPath != "" {
		return c.ChatPath
	}
	return "/chat/completions"
}

// modelsPath 默认 /models
func (c providerConfig) modelsPath() string {
	if c.ModelsPath != "" {
		return c.ModelsPath
	}
	return "/models"
}

// freeSet 白名单集合
func (c providerConfig) freeSet() map[string]bool {
	s := make(map[string]bool, len(c.FreeModels))
	for _, m := range c.FreeModels {
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

	mu          sync.Mutex
	catalog     map[string]*catalogModel
	slugs       map[string]*catalogModel
	fetchedAt   int64 // unix ms
	attemptedAt int64
	catalogErr  string
	rejected    map[string]string // modelID -> 永久拒绝原因
	sigCache    *thoughtSignatureCache
}

func newModelProvider(name string) *modelProvider {
	return &modelProvider{
		name:     name,
		rejected: map[string]string{},
		sigCache: newThoughtSignatureCache(5000),
	}
}

// providerByName 按名取运行时状态; 配置中不存在则返回 nil。
func providerByName(name string) *modelProvider {
	cfg := getZenConfig()
	if cfg == nil {
		return nil
	}
	if _, ok := cfg.Providers[name]; !ok {
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
	cfg := getZenConfig()
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// validateProviderConfig 校验 provider 配置。
func validateProviderConfig(name string, c providerConfig) error {
	if !providerIDRe.MatchString(name) {
		return fmt.Errorf("provider id %q must match %s", name, providerIDRe)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("provider %s: baseUrl is required", name)
	}
	return nil
}

// mutateProvidersConfig 锁内修改全局配置并落盘。
func mutateProvidersConfig(fn func(cfg *zenConfigData)) {
	zenConfigMu.Lock()
	fn(zenConfig)
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
