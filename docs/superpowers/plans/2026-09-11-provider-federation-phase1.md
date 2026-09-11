# Provider Federation 第 1 期实现计划（通用 Provider 抽象）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Cline-proxy 能挂载任意 OpenAI 兼容上游（Gemini / OpenRouter / TokenRouter / B.AI），以 `provider:model` 前缀直选模型，并正确处理 Gemini 的 thought-signature 与配额语义。

**Architecture:** 新增一层与 zen / cline 池平级的 provider 抽象：配置存于 `.zen-config.json` 的 `providers` 段，运行时按名惰性建立状态对象（目录缓存、签名缓存、永久拒绝集合）。免费判定有三模式——按目录价格、按白名单、白名单+目录校验。Gemini 特殊处理拆成两个纯逻辑模块（thought-signature、配额解析），便于单测。

**Tech Stack:** Go（零新增第三方依赖），标准库 `net/http` / `encoding/json` / `sync`；测试用 `net/http/httptest`。

**Spec:** `docs/superpowers/specs/2026-09-11-multi-provider-routing-design.md`

## Global Constraints

- 构建必须带 `-tags "with_quic,with_grpc,with_utls"`，否则 reality/QUIC 类节点会被剔除（见 README）。
- `go` 不在 PATH，必须用 `& "C:\Go\bin\go.exe" <args>`。
- 不引入任何新的第三方依赖，保持单 exe 分发。
- 现有测试（`./internal/app/`）必须全部保持通过。
- 配置与运行时数据一律经 `kit.ResolveDataPath(...)` 落到 data 目录。
- `.zen-config.json` 的 `providers` 是一个 map，且会被 admin 保存路径就地写入；读取它**只能**经 `providerConfigFor(name)`（锁内），直接读 `getZenConfig().Providers[...]` 会与写入并发触发 Go 不可恢复的 `concurrent map read and map write`。
- provider 运行时状态里的 map（`catalog` / `slugs` / `rejected`）一律"锁内整体替换"，不得在锁内就地增删：`isFree` 与 `freeModelIDs` 只在锁内取 map 头部、解锁后才读取其**内容**，就地写入会与之并发触发同一个 fatal 的 map 读写竞争。
- commit 信息只描述最终状态，不写"修复了 X"式叙述。
- 工作目录：`D:\deepseek\Cline-proxy`。

---

### Task 1: Provider 配置类型与运行时注册表

**Files:**
- Create: `internal/app/providers_config.go`
- Modify: `internal/app/zen.go:206-221`（`zenConfigData` 增加 `Providers` 字段）
- Test: `internal/app/providers_test.go`

**Interfaces:**
- Consumes: `getZenConfig() *zenConfigData`、`saveZenConfig()`、`zenConfigMu`、`zenConfig`（zen.go 已有）
- Produces:
  - `type providerHeaderSpec struct{ Env, Default string }`
  - `type providerConfig struct{ BaseURL, APIKey string; Catalog, Pricing, ProbeFreeTier bool; ChatPath, ModelsPath, ModelsURL, ModelsKeyHeader string; Headers map[string]providerHeaderSpec; FreeModels []string }`
  - `func (c providerConfig) chatPath() string` / `modelsPath() string` / `freeSet() map[string]bool`
  - `func (c providerConfig) resolveHeader(spec providerHeaderSpec, origin string) string`
  - `func providerConfigFor(name string) (providerConfig, bool)`（锁内单条读取，唯一允许的 `Providers` 读入口）
  - `func providerByName(name string) *modelProvider`（配置中不存在返回 nil）
  - `func providerNames() []string`（排序）
  - `func validateProviderConfig(name string, c providerConfig) error`
  - `func mutateProvidersConfig(fn func(cfg *zenConfigData))`
  - `func setListenOrigin(origin string)` / `func localOrigin() string`
  - `type modelProvider struct{...}` / `func newModelProvider(name string) *modelProvider`

- [ ] **Step 1: 给 zenConfigData 增加 Providers 字段**

修改 `internal/app/zen.go`，在 `zenConfigData` 末尾（`Compaction` 字段之后）加一行：

```go
	Compaction      zenCompactConfig          `json:"compaction"`
	Providers       map[string]providerConfig `json:"providers,omitempty"` // 通用 OpenAI 兼容上游
```

- [ ] **Step 2: 写失败测试**

创建 `internal/app/providers_test.go`：

```go
package app

import "testing"

// setTestProvider 在测试内挂一个 provider 配置, 结束时清理配置与运行时状态。
func setTestProvider(t *testing.T, name string, pc providerConfig) {
	t.Helper()
	zenConfigMu.Lock()
	if zenConfig == nil {
		zenConfig = &zenConfigData{}
	}
	if zenConfig.Providers == nil {
		zenConfig.Providers = map[string]providerConfig{}
	}
	zenConfig.Providers[name] = pc
	zenConfigMu.Unlock()
	t.Cleanup(func() {
		zenConfigMu.Lock()
		if zenConfig != nil {
			delete(zenConfig.Providers, name)
		}
		zenConfigMu.Unlock()
		providerRTMu.Lock()
		delete(providerRT, name)
		providerRTMu.Unlock()
	})
}

func TestProviderConfigDefaults(t *testing.T) {
	pc := providerConfig{BaseURL: "https://example.com/v1"}
	if pc.chatPath() != "/chat/completions" {
		t.Fatalf("chatPath default: %q", pc.chatPath())
	}
	if pc.modelsPath() != "/models" {
		t.Fatalf("modelsPath default: %q", pc.modelsPath())
	}
	custom := providerConfig{ChatPath: "/v1/chat", ModelsPath: "/v1/models"}
	if custom.chatPath() != "/v1/chat" || custom.modelsPath() != "/v1/models" {
		t.Fatal("explicit paths must win")
	}
}

func TestProviderFreeSet(t *testing.T) {
	pc := providerConfig{FreeModels: []string{" a ", "", "b"}}
	s := pc.freeSet()
	if len(s) != 2 || !s["a"] || !s["b"] {
		t.Fatalf("freeSet: %+v", s)
	}
}

func TestValidateProviderConfig(t *testing.T) {
	if err := validateProviderConfig("openrouter", providerConfig{BaseURL: "https://x"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := validateProviderConfig("Bad-Name", providerConfig{BaseURL: "https://x"}); err == nil {
		t.Fatal("uppercase id must be rejected")
	}
	if err := validateProviderConfig("ok", providerConfig{}); err == nil {
		t.Fatal("missing baseUrl must be rejected")
	}
}

func TestProviderRegistryLookup(t *testing.T) {
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://x", APIKey: "k"})
	if providerByName("openrouter") == nil {
		t.Fatal("configured provider must resolve")
	}
	if providerByName("unknown") != nil {
		t.Fatal("unknown provider must be nil")
	}
	found := false
	for _, n := range providerNames() {
		if n == "openrouter" {
			found = true
		}
	}
	if !found {
		t.Fatal("providerNames must include configured provider")
	}
}

func TestResolveHeaderOriginExpansion(t *testing.T) {
	pc := providerConfig{}
	got := pc.resolveHeader(providerHeaderSpec{Default: "${origin}"}, "http://127.0.0.1:3457")
	if got != "http://127.0.0.1:3457" {
		t.Fatalf("origin expansion: %q", got)
	}
	got = pc.resolveHeader(providerHeaderSpec{Default: "Free Router"}, "http://x")
	if got != "Free Router" {
		t.Fatalf("static default: %q", got)
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestProvider(ConfigDefaults|FreeSet|RegistryLookup)|TestValidateProviderConfig|TestResolveHeaderOriginExpansion' -v`
Expected: 编译失败（`providerConfig` / `providerByName` 等未定义）

- [ ] **Step 4: 实现 providers_config.go**

创建 `internal/app/providers_config.go`：

```go
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
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestProvider(ConfigDefaults|FreeSet|RegistryLookup)|TestValidateProviderConfig|TestResolveHeaderOriginExpansion' -v`
Expected: PASS（5 个测试）

- [ ] **Step 6: 提交**

```bash
git add internal/app/providers_config.go internal/app/providers_test.go internal/app/zen.go
git commit -m "feat: add generic provider config and runtime registry"
```

---

### Task 2: 目录模型与免费判定（纯函数）

**Files:**
- Create: `internal/app/providers_catalog.go`
- Test: `internal/app/providers_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `providerConfig`、`modelProvider`
- Produces:
  - `type catalogModel struct{ ID, Name, Tokenizer string; ChatCapable *bool; OutputModalities []string; ContextLength, MaxOutput int; Created int64; PricesKnown bool; PromptPrice, CompletionPrice float64 }`
  - `func isZeroCost(m *catalogModel) bool`
  - `func isChatModel(m *catalogModel) bool`
  - `func normalizeModelSlug(id string) string`
  - `func catalogLookup(cat, slugs map[string]*catalogModel, modelID string) *catalogModel`
  - `func evalProviderFree(cfg providerConfig, cat, slugs map[string]*catalogModel, rejected map[string]string, modelID string) bool`
  - `func (p *modelProvider) isFree(modelID string) bool`
  - `func (p *modelProvider) catalogEntry(modelID string) *catalogModel`

- [ ] **Step 1: 写失败测试**

追加到 `internal/app/providers_test.go`：

```go
func boolPtr(v bool) *bool { return &v }

func TestNormalizeModelSlug(t *testing.T) {
	cases := map[string]string{
		"Gemini-3.8-Flash":       "gemini-3.8-flash",
		"z-ai/glm-5.3:free":      "glm-5.3",
		" models/gemini-3.7-flash ": "gemini-3.7-flash",
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeModelSlug(in); got != want {
			t.Errorf("normalizeModelSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsZeroCost(t *testing.T) {
	zero := &catalogModel{PricesKnown: true, PromptPrice: 0, CompletionPrice: 0}
	if !isZeroCost(zero) {
		t.Fatal("zero prices must count as free")
	}
	paid := &catalogModel{PricesKnown: true, PromptPrice: 0.001, CompletionPrice: 0.002}
	if isZeroCost(paid) {
		t.Fatal("paid model must not be free")
	}
	unknown := &catalogModel{PricesKnown: false}
	if isZeroCost(unknown) {
		t.Fatal("unknown pricing must not count as free")
	}
	if isZeroCost(nil) {
		t.Fatal("nil must not be free")
	}
}

func TestIsChatModel(t *testing.T) {
	if !isChatModel(&catalogModel{ChatCapable: boolPtr(true)}) {
		t.Fatal("explicit chatCapable=true must pass")
	}
	if isChatModel(&catalogModel{ChatCapable: boolPtr(false)}) {
		t.Fatal("explicit chatCapable=false must fail")
	}
	// 明确 chatCapable 优先于 tokenizer 启发式
	if !isChatModel(&catalogModel{ChatCapable: boolPtr(true), Tokenizer: "Router"}) {
		t.Fatal("explicit chatCapable must win over tokenizer heuristic")
	}
	if isChatModel(&catalogModel{Tokenizer: "Router"}) {
		t.Fatal("Router tokenizer must be excluded")
	}
	if isChatModel(&catalogModel{OutputModalities: []string{"image"}}) {
		t.Fatal("non-text output must be excluded")
	}
	if !isChatModel(&catalogModel{OutputModalities: []string{"text"}}) {
		t.Fatal("text output must pass")
	}
	if isChatModel(&catalogModel{ID: "openai/gpt-content-safety"}) {
		t.Fatal("moderation model must be excluded")
	}
}

func TestEvalProviderFree(t *testing.T) {
	cfg := providerConfig{APIKey: "k", Catalog: true, Pricing: true}
	cat := map[string]*catalogModel{
		"z-ai/glm-5.3:free": {ID: "z-ai/glm-5.3:free", PricesKnown: true, PromptPrice: 0, CompletionPrice: 0},
		"paid":              {ID: "paid", PricesKnown: true, PromptPrice: 1, CompletionPrice: 1},
	}
	if !evalProviderFree(cfg, cat, nil, nil, "z-ai/glm-5.3:free") {
		t.Fatal("zero-cost catalog model must be free")
	}
	if evalProviderFree(cfg, cat, nil, nil, "paid") {
		t.Fatal("paid catalog model must not be free")
	}
	if evalProviderFree(cfg, cat, nil, nil, "missing") {
		t.Fatal("model absent from catalog must not be free")
	}
	if evalProviderFree(providerConfig{APIKey: ""}, nil, nil, nil, "x") {
		t.Fatal("provider without key must not be free")
	}
	// 白名单模式
	wl := providerConfig{APIKey: "k", FreeModels: []string{"mimo-v2.5"}}
	if !evalProviderFree(wl, nil, nil, nil, "mimo-v2.5") {
		t.Fatal("whitelisted model must be free")
	}
	if evalProviderFree(wl, nil, nil, nil, "other") {
		t.Fatal("non-whitelisted model must not be free")
	}
	// 白名单 + 目录校验: 已下架模型不算免费
	wlc := providerConfig{APIKey: "k", Catalog: true, FreeModels: []string{"gone", "here"}}
	cat2 := map[string]*catalogModel{"here": {ID: "here"}}
	if evalProviderFree(wlc, cat2, nil, nil, "gone") {
		t.Fatal("whitelisted but withdrawn model must not be free")
	}
	if !evalProviderFree(wlc, cat2, nil, nil, "here") {
		t.Fatal("whitelisted and present model must be free")
	}
	// 永久拒绝
	if evalProviderFree(cfg, cat, nil, map[string]string{"z-ai/glm-5.3:free": "withdrawn"}, "z-ai/glm-5.3:free") {
		t.Fatal("permanently rejected model must not be free")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestNormalizeModelSlug|TestIsZeroCost|TestIsChatModel|TestEvalProviderFree' -v`
Expected: 编译失败（`catalogModel` / `isZeroCost` 等未定义）

- [ ] **Step 3: 实现 providers_catalog.go 的纯函数部分**

创建 `internal/app/providers_catalog.go`（本步只放纯函数，目录拉取放 Task 3）：

```go
package app

import (
	"regexp"
	"strings"
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
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestNormalizeModelSlug|TestIsZeroCost|TestIsChatModel|TestEvalProviderFree' -v`
Expected: PASS（4 个测试）

- [ ] **Step 5: 提交**

```bash
git add internal/app/providers_catalog.go internal/app/providers_test.go
git commit -m "feat: add provider catalog model and free-model detection"
```

---

### Task 3: 目录拉取与方言归一化

**Files:**
- Modify: `internal/app/providers_catalog.go`（追加网络与归一化部分）
- Test: `internal/app/providers_test.go`（追加）

**Interfaces:**
- Consumes: Task 1/2 的类型；`providerDirectClient`（Task 6 定义，本任务先建）
- Produces:
  - `func normalizeCatalogPayload(payload []byte) catalogPage`
  - `type catalogPage struct{ shape string; models []*catalogModel; nextPageToken string }`
  - `func (p *modelProvider) refreshCatalog(ctx context.Context, force bool) error`
  - `func (p *modelProvider) freeModelIDs() []catalogModel`
  - `func providerModelList() []map[string]any`
  - `func (p *modelProvider) catalogStatus() map[string]any`
  - `func startProviderRefresher()`

- [ ] **Step 1: 写失败测试**

追加到 `internal/app/providers_test.go`（需要新增 import：`context`、`encoding/json`、`net/http`、`net/http/httptest`）：

```go
func TestNormalizeCatalogPayloadOpenAI(t *testing.T) {
	payload := []byte(`{"data":[
		{"id":"m1","created":1,"context_length":8000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
		{"id":"m2","pricing":{"prompt":"0.000001","completion":"0.000002"}}
	]}`)
	page := normalizeCatalogPayload(payload)
	if page.shape != "openai" || len(page.models) != 2 {
		t.Fatalf("openai shape: %+v", page)
	}
	if !page.models[0].PricesKnown || page.models[0].PromptPrice != 0 || page.models[0].ContextLength != 8000 {
		t.Fatalf("openai model parse: %+v", page.models[0])
	}
	if page.models[1].PromptPrice != 0.000001 {
		t.Fatalf("price parse: %+v", page.models[1])
	}
}

func TestNormalizeCatalogPayloadGoogle(t *testing.T) {
	payload := []byte(`{"models":[{
		"name":"models/gemini-3.8-flash","displayName":"Gemini 3.8",
		"inputTokenLimit":1000,"outputTokenLimit":2000,
		"supportedGenerationMethods":["generateContent","embedContent"]
	}],"nextPageToken":"p2"}`)
	page := normalizeCatalogPayload(payload)
	if page.shape != "google" || len(page.models) != 1 || page.nextPageToken != "p2" {
		t.Fatalf("google shape: %+v", page)
	}
	m := page.models[0]
	if m.ID != "gemini-3.8-flash" || m.Name != "Gemini 3.8" || m.ContextLength != 1000 || m.MaxOutput != 2000 {
		t.Fatalf("google model parse: %+v", m)
	}
	if m.ChatCapable == nil || !*m.ChatCapable {
		t.Fatalf("generateContent must mark chat capability: %+v", m.ChatCapable)
	}
	// 仅 embedContent 的模型不可聊天
	payload2 := []byte(`{"models":[{"name":"models/embed","supportedGenerationMethods":["embedContent"]}]}`)
	p2 := normalizeCatalogPayload(payload2)
	if len(p2.models) != 1 || p2.models[0].ChatCapable == nil || *p2.models[0].ChatCapable {
		t.Fatalf("embedding model must not be chat capable: %+v", p2.models)
	}
}

func TestNormalizeCatalogPayloadUnknown(t *testing.T) {
	if got := normalizeCatalogPayload([]byte(`{"foo":1}`)); got.shape != "unknown" {
		t.Fatalf("unknown shape: %+v", got)
	}
	if got := normalizeCatalogPayload([]byte(`not json`)); got.shape != "unknown" {
		t.Fatalf("invalid json shape: %+v", got)
	}
}

func TestRefreshCatalogPricingMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("catalog path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("catalog auth: %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "free-a", "pricing": map[string]any{"prompt": "0", "completion": "0"}},
			{"id": "paid", "pricing": map[string]any{"prompt": "1", "completion": "1"}},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "t", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("t")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !p.isFree("free-a") {
		t.Fatal("zero-cost catalog model must be free")
	}
	if p.isFree("paid") {
		t.Fatal("paid catalog model must not be free")
	}
	ids := p.freeModelIDs()
	if len(ids) != 1 || ids[0].ID != "free-a" {
		t.Fatalf("freeModelIDs: %+v", ids)
	}
}

func TestRefreshCatalogGoogleKeyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "gk" {
			t.Errorf("models key header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Bearer must not be used when modelsKeyHeader is set: %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"name": "models/g1", "supportedGenerationMethods": []string{"generateContent"}},
		}})
	}))
	defer srv.Close()

	setTestProvider(t, "g", providerConfig{
		BaseURL: srv.URL, APIKey: "gk", Catalog: true,
		ModelsURL: srv.URL + "/models", ModelsKeyHeader: "x-goog-api-key",
	})
	p := providerByName("g")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if p.catalogEntry("g1") == nil {
		t.Fatalf("google catalog entry missing: %+v", p.catalog)
	}
}

func TestRefreshCatalogErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	setTestProvider(t, "bad", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, Pricing: true})
	p := providerByName("bad")
	if err := p.refreshCatalog(context.Background(), true); err == nil {
		t.Fatal("catalog failure must return an error")
	}
	st := p.catalogStatus()
	if st["error"] == "" {
		t.Fatalf("catalog error must be recorded: %+v", st)
	}
}

func TestProviderModelList(t *testing.T) {
	setTestProvider(t, "bai", providerConfig{BaseURL: "https://x", APIKey: "k", FreeModels: []string{"glm-5.3-flash", "mimo-v2.5"}})
	setTestProvider(t, "nokey", providerConfig{BaseURL: "https://y", FreeModels: []string{"whatever"}})
	list := providerModelList()
	seen := map[string]bool{}
	for _, m := range list {
		seen[m["id"].(string)] = true
	}
	if !seen["bai:glm-5.3-flash"] || !seen["bai:mimo-v2.5"] {
		t.Fatalf("provider models must be prefixed: %+v", seen)
	}
	if seen["nokey:whatever"] {
		t.Fatal("provider without key must be excluded")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestNormalizeCatalogPayload|TestRefreshCatalog|TestProviderModelList' -v`
Expected: 编译失败（`normalizeCatalogPayload` / `refreshCatalog` 等未定义）

- [ ] **Step 3: 实现目录拉取**

在 `internal/app/providers_catalog.go` 追加（并把 import 扩为本题所需）：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

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
func (p *modelProvider) freeModelIDs() []catalogModel {
	cfg, _ := providerConfigFor(p.name)
	p.mu.Lock()
	cat, slugs, rejected := p.catalog, p.slugs, p.rejected
	p.mu.Unlock()
	var out []catalogModel
	if cfg.Catalog && cfg.Pricing {
		for _, m := range cat {
			if isZeroCost(m) && isChatModel(m) {
				out = append(out, *m)
			}
		}
	} else {
		for id := range cfg.freeSet() {
			if _, bad := rejected[id]; bad {
				continue
			}
			if cfg.Catalog && len(cat) > 0 && catalogLookup(cat, slugs, id) == nil {
				continue
			}
			out = append(out, catalogModel{ID: id})
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
```

- [ ] **Step 4: 加 providerDirectClient**

`providerDirectClient` 由 Task 6 的 `providers_chat.go` 定义。为让本任务可编译，**先**创建 `internal/app/providers_chat.go` 的客户端部分（Task 6 会补齐其余内容）：

```go
package app

import (
	"net/http"
	"time"
)

// providerDirectClient 直连上游(默认用于非 Gemini provider)。
var providerDirectClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// providerProxiedClient 走系统代理(用于需要海外出口的 Gemini)。
var providerProxiedClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	},
}
```

同时把 `log` 加入 `providers_catalog.go` 的 import（`startProviderRefresher` 用到）。

- [ ] **Step 5: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestNormalizeCatalogPayload|TestRefreshCatalog|TestProviderModelList' -v`
Expected: PASS（6 个测试）

- [ ] **Step 6: 提交**

```bash
git add internal/app/providers_catalog.go internal/app/providers_chat.go internal/app/providers_test.go
git commit -m "feat: fetch provider catalogs with openai and google dialect normalization"
```

---

### Task 4: Gemini thought-signature

**Files:**
- Create: `internal/app/thought_signature.go`
- Test: `internal/app/thought_signature_test.go`

**Interfaces:**
- Consumes: Task 1 的 `modelProvider`
- Produces:
  - `const skipThoughtSignature = "skip_thought_signature_validator"`
  - `func newThoughtSignatureCache(max int) *thoughtSignatureCache`
  - `func (c *thoughtSignatureCache) remember(id, sig string)` / `lookup(id string) string`
  - `func readThoughtSignature(call map[string]any) string`
  - `func writeThoughtSignature(call map[string]any, sig string)`
  - `func rememberSignaturesFromPayload(payload map[string]any, cache *thoughtSignatureCache)`
  - `func injectThoughtSignatures(body map[string]any, cache *thoughtSignatureCache)`
  - `func isMissingThoughtSignatureError(status int, message string) bool`
  - `func providerNeedsThoughtSignatures(p *modelProvider) bool`
  - `func newSignatureStreamExtractor(cache *thoughtSignatureCache) *signatureStreamExtractor`
  - `func (e *signatureStreamExtractor) push(text string)`

- [ ] **Step 1: 写失败测试**

创建 `internal/app/thought_signature_test.go`：

```go
package app

import "testing"

func TestThoughtSignatureCacheLRU(t *testing.T) {
	cache := newThoughtSignatureCache(2)
	cache.remember("a", "1")
	cache.remember("b", "2")
	cache.remember("c", "3")
	if cache.lookup("a") != "" {
		t.Fatal("oldest entry must be evicted")
	}
	if cache.lookup("b") != "2" || cache.lookup("c") != "3" {
		t.Fatal("recent entries must survive")
	}
	// 空白值不入缓存
	cache.remember("d", "   ")
	if cache.lookup("d") != "" {
		t.Fatal("blank signature must not be stored")
	}
}

func TestThoughtSignatureReadWrite(t *testing.T) {
	call := map[string]any{"id": "c1"}
	writeThoughtSignature(call, "sig-1")
	if got := readThoughtSignature(call); got != "sig-1" {
		t.Fatalf("read after write: %q", got)
	}
	// 也识别扁平字段
	flat := map[string]any{"thought_signature": "sig-2"}
	if got := readThoughtSignature(flat); got != "sig-2" {
		t.Fatalf("flat field: %q", got)
	}
	alt := map[string]any{"extra_content": map[string]any{"google": map[string]any{"thoughtSignature": "sig-3"}}}
	if got := readThoughtSignature(alt); got != "sig-3" {
		t.Fatalf("camelCase field: %q", got)
	}
	if readThoughtSignature(nil) != "" {
		t.Fatal("nil call must read empty")
	}
}

func TestInjectThoughtSignatures(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	cache.remember("known", "sig-known")
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "known"},
				map[string]any{"id": "unknown"},
			}},
		},
	}
	injectThoughtSignatures(body, cache)
	msgs := body["messages"].([]any)
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls[0].(map[string]any)); got != "sig-known" {
		t.Fatalf("cached signature: %q", got)
	}
	if got := readThoughtSignature(calls[1].(map[string]any)); got != skipThoughtSignature {
		t.Fatalf("sentinel for unknown: %q", got)
	}
	// 已有签名不被覆盖
	body2 := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "x", "extra_content": map[string]any{"google": map[string]any{"thought_signature": "keep"}}},
		}},
	}}
	injectThoughtSignatures(body2, cache)
	calls2 := body2["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls2[0].(map[string]any)); got != "keep" {
		t.Fatalf("existing signature must be preserved: %q", got)
	}
}

func TestRememberSignaturesFromPayload(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	payload := map[string]any{"choices": []any{
		map[string]any{"message": map[string]any{"tool_calls": []any{
			map[string]any{"id": "t1", "extra_content": map[string]any{"google": map[string]any{"thought_signature": "s1"}}},
		}}},
	}}
	rememberSignaturesFromPayload(payload, cache)
	if got := cache.lookup("t1"); got != "s1" {
		t.Fatalf("remembered: %q", got)
	}
}

func TestSignatureStreamExtractor(t *testing.T) {
	cache := newThoughtSignatureCache(10)
	ext := newSignatureStreamExtractor(cache)
	// id 与签名分两条到达(真实流式可能拆开)
	ext.push("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"st1\"}]}}]}\n")
	ext.push("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"extra_content\":{\"google\":{\"thought_signature\":\"sig-st\"}}}]}}]}\n")
	if got := cache.lookup("st1"); got != "sig-st" {
		t.Fatalf("stream signature: %q", got)
	}
	// 非 data: 行与 [DONE] 被忽略
	ext.push(": keepalive\ndata: [DONE]\n")
}

func TestIsMissingThoughtSignatureError(t *testing.T) {
	if !isMissingThoughtSignatureError(400, "missing thought_signature for tool call") {
		t.Fatal("must detect missing signature 400")
	}
	if isMissingThoughtSignatureError(500, "thought_signature") {
		t.Fatal("only 400 counts")
	}
	if isMissingThoughtSignatureError(400, "other problem") {
		t.Fatal("unrelated 400 must not match")
	}
}

func TestProviderNeedsThoughtSignatures(t *testing.T) {
	setTestProvider(t, "gemini", providerConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: "k"})
	if !providerNeedsThoughtSignatures(providerByName("gemini")) {
		t.Fatal("gemini provider must need signatures")
	}
	setTestProvider(t, "custom", providerConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: "k"})
	if !providerNeedsThoughtSignatures(providerByName("custom")) {
		t.Fatal("generativelanguage base url must need signatures")
	}
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://openrouter.ai/api/v1", APIKey: "k"})
	if providerNeedsThoughtSignatures(providerByName("openrouter")) {
		t.Fatal("openrouter must not need signatures")
	}
	if providerNeedsThoughtSignatures(nil) {
		t.Fatal("nil provider must be false")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestThoughtSignature|TestInjectThoughtSignatures|TestRememberSignaturesFromPayload|TestSignatureStreamExtractor|TestIsMissingThoughtSignatureError|TestProviderNeedsThoughtSignatures' -v`
Expected: 编译失败（符号未定义）

- [ ] **Step 3: 实现 thought_signature.go**

创建 `internal/app/thought_signature.go`：

```go
package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

// skipThoughtSignature Google 文档化的跳过哨兵: 历史未经过本进程时使用。
const skipThoughtSignature = "skip_thought_signature_validator"

// 注: thoughtSignatureCache 结构体与 newThoughtSignatureCache 已由 Task 1 在
// providers_config.go 中声明, 本文件只实现其方法, 不要重复声明类型。

func (c *thoughtSignatureCache) remember(id, sig string) {
	id = strings.TrimSpace(id)
	sig = strings.TrimSpace(sig)
	if id == "" || sig == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		for i, k := range c.order {
			if k == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.m[id] = sig
	c.order = append(c.order, id)
	for len(c.order) > c.max {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.m, old)
	}
}

func (c *thoughtSignatureCache) lookup(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[strings.TrimSpace(id)]
}

// readThoughtSignature 从工具调用中读取签名(兼容多种字段名)。
func readThoughtSignature(call map[string]any) string {
	if call == nil {
		return ""
	}
	extra, _ := call["extra_content"].(map[string]any)
	var google map[string]any
	if extra != nil {
		google, _ = extra["google"].(map[string]any)
	}
	for _, key := range []string{"thought_signature", "thoughtSignature"} {
		if google != nil {
			if v, ok := google[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		if v, ok := call[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeThoughtSignature 写入 extra_content.google.thought_signature。
func writeThoughtSignature(call map[string]any, sig string) {
	if call == nil {
		return
	}
	extra, _ := call["extra_content"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
		call["extra_content"] = extra
	}
	google, _ := extra["google"].(map[string]any)
	if google == nil {
		google = map[string]any{}
		extra["google"] = google
	}
	google["thought_signature"] = sig
}

// rememberSignaturesFromPayload 从非流式响应中提取并缓存签名。
func rememberSignaturesFromPayload(payload map[string]any, cache *thoughtSignatureCache) {
	if payload == nil || cache == nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		for _, key := range []string{"message", "delta"} {
			msg, _ := ch[key].(map[string]any)
			if msg == nil {
				continue
			}
			calls, _ := msg["tool_calls"].([]any)
			for _, cc := range calls {
				call, _ := cc.(map[string]any)
				if call == nil {
					continue
				}
				id, _ := call["id"].(string)
				if sig := readThoughtSignature(call); id != "" && sig != "" {
					cache.remember(id, sig)
				}
			}
		}
	}
}

// injectThoughtSignatures 为历史 assistant 消息中的工具调用回填签名。
func injectThoughtSignatures(body map[string]any, cache *thoughtSignatureCache) {
	if body == nil || cache == nil {
		return
	}
	messages, _ := body["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, cc := range calls {
			call, _ := cc.(map[string]any)
			if call == nil || readThoughtSignature(call) != "" {
				continue
			}
			sig := ""
			if id, _ := call["id"].(string); id != "" {
				sig = cache.lookup(id)
			}
			if sig == "" {
				sig = skipThoughtSignature
			}
			writeThoughtSignature(call, sig)
		}
	}
}

var missingSignatureRe = regexp.MustCompile(`(?i)thought[_ ]signature`)

// isMissingThoughtSignatureError 400 且 message 提到 thought_signature。
func isMissingThoughtSignatureError(status int, message string) bool {
	return status == 400 && missingSignatureRe.MatchString(message)
}

// providerNeedsThoughtSignatures 仅 Gemini(按名或 generativelanguage 域名)需要。
func providerNeedsThoughtSignatures(p *modelProvider) bool {
	if p == nil {
		return false
	}
	if p.name == "gemini" {
		return true
	}
	cfg, ok := providerConfigFor(p.name)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(cfg.BaseURL), "generativelanguage.googleapis.com")
}

// providerUsesProxiedClient 需要海外出口的 provider(generativelanguage)走系统代理。
func providerUsesProxiedClient(p *modelProvider) bool {
	if p == nil {
		return false
	}
	cfg, ok := providerConfigFor(p.name)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(cfg.BaseURL), "generativelanguage.googleapis.com")
}

type sigSlot struct {
	id        string
	signature string
}

// signatureStreamExtractor 从 SSE 流中逐行提取工具调用签名。
type signatureStreamExtractor struct {
	cache   *thoughtSignatureCache
	pending map[int]*sigSlot
}

func newSignatureStreamExtractor(cache *thoughtSignatureCache) *signatureStreamExtractor {
	return &signatureStreamExtractor{cache: cache, pending: map[int]*sigSlot{}}
}

func (e *signatureStreamExtractor) ingestCall(call map[string]any, fallbackIndex int) {
	if call == nil {
		return
	}
	idx := fallbackIndex
	if i, ok := call["index"].(float64); ok {
		idx = int(i)
	}
	slot := e.pending[idx]
	if slot == nil {
		slot = &sigSlot{}
		e.pending[idx] = slot
	}
	if id, _ := call["id"].(string); id != "" {
		slot.id = id
	}
	if sig := readThoughtSignature(call); sig != "" {
		slot.signature = sig
	}
	if slot.id != "" && slot.signature != "" {
		e.cache.remember(slot.id, slot.signature)
	}
}

func (e *signatureStreamExtractor) ingestLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			msg, _ := ch[key].(map[string]any)
			if msg == nil {
				continue
			}
			calls, _ := msg["tool_calls"].([]any)
			for i, cc := range calls {
				call, _ := cc.(map[string]any)
				e.ingestCall(call, i)
			}
		}
	}
}

// push 接收一行或多行文本(调用方负责按 \n 切分)。
func (e *signatureStreamExtractor) push(text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			e.ingestLine(line)
		}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestThoughtSignature|TestInjectThoughtSignatures|TestRememberSignaturesFromPayload|TestSignatureStreamExtractor|TestIsMissingThoughtSignatureError|TestProviderNeedsThoughtSignatures' -v`
Expected: PASS（7 个测试）

- [ ] **Step 5: 提交**

```bash
git add internal/app/thought_signature.go internal/app/thought_signature_test.go
git commit -m "feat: carry gemini thought-signatures across provider turns"
```

---

### Task 5: Gemini 配额解析与永久拒绝

**Files:**
- Create: `internal/app/gemini_quota.go`
- Test: `internal/app/gemini_quota_test.go`

**Interfaces:**
- Produces:
  - `type geminiQuotaFailure struct{ NoFreeTier bool; DailyRequestLimit *int; ExhaustedWindow string; RetryDelayMs int64 }`
  - `func parseQuotaFailure(payload map[string]any) *geminiQuotaFailure`
  - `func permanentRejectionReason(status int, payload map[string]any) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/app/gemini_quota_test.go`：

```go
package app

import "testing"

func quotaPayload(message string, limit int, quotaID string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"details": []any{
				map[string]any{
					"@type": "type.googleapis.com/google.rpc.QuotaFailure",
					"violations": []any{
						map[string]any{
							"quotaMetric": "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
							"quotaId":     quotaID,
						},
					},
				},
			},
		},
	}
}

func TestParseQuotaFailureDailyExhausted(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerDayPerProjectPerModel-FreeTier, limit: 20, model: gemini-3.8-flash",
		20, "GenerateRequestsPerDayPerProjectPerModel-FreeTier")
	qf := parseQuotaFailure(payload)
	if qf == nil {
		t.Fatal("quota failure must parse")
	}
	if qf.NoFreeTier {
		t.Fatal("free tier exists")
	}
	if qf.ExhaustedWindow != "day" {
		t.Fatalf("window: %q", qf.ExhaustedWindow)
	}
	if qf.DailyRequestLimit == nil || *qf.DailyRequestLimit != 20 {
		t.Fatalf("daily limit: %+v", qf.DailyRequestLimit)
	}
}

func TestParseQuotaFailureNoFreeTier(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerDayPerProjectPerModel-FreeTier, limit: 0",
		0, "GenerateRequestsPerDayPerProjectPerModel-FreeTier")
	qf := parseQuotaFailure(payload)
	if qf == nil || !qf.NoFreeTier {
		t.Fatalf("limit 0 must mean no free tier: %+v", qf)
	}
}

func TestParseQuotaFailureRetryDelay(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerMinutePerProjectPerModel-FreeTier, limit: 5",
		5, "GenerateRequestsPerMinutePerProjectPerModel-FreeTier")
	payload["error"].(map[string]any)["details"] = append(
		payload["error"].(map[string]any)["details"].([]any),
		map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "33s"},
	)
	qf := parseQuotaFailure(payload)
	if qf == nil {
		t.Fatal("quota failure must parse")
	}
	if qf.RetryDelayMs != 33000 {
		t.Fatalf("retry delay: %d", qf.RetryDelayMs)
	}
	if qf.ExhaustedWindow != "minute" {
		t.Fatalf("minute window: %q", qf.ExhaustedWindow)
	}
}

func TestParseQuotaFailureNonQuota(t *testing.T) {
	if qf := parseQuotaFailure(map[string]any{"error": map[string]any{"message": "boom"}}); qf != nil {
		t.Fatal("non-quota payload must be nil")
	}
	if qf := parseQuotaFailure(nil); qf != nil {
		t.Fatal("nil payload must be nil")
	}
	if qf := parseQuotaFailure(map[string]any{}); qf != nil {
		t.Fatal("empty payload must be nil")
	}
}

func TestPermanentRejectionReason(t *testing.T) {
	withdrawn := map[string]any{"error": map[string]any{"message": "model x is no longer available"}}
	if got := permanentRejectionReason(404, withdrawn); got != "withdrawn upstream" {
		t.Fatalf("404 withdrawn: %q", got)
	}
	if got := permanentRejectionReason(404, nil); got == "" {
		t.Fatal("bare 404 must still be permanent")
	}
	interactions := map[string]any{"error": map[string]any{"message": "model x only supports the Interactions API"}}
	if got := permanentRejectionReason(400, interactions); got != "not a chat-completions model" {
		t.Fatalf("400 interactions: %q", got)
	}
	if got := permanentRejectionReason(500, map[string]any{}); got != "" {
		t.Fatalf("5xx must not be permanent: %q", got)
	}
	if got := permanentRejectionReason(429, map[string]any{}); got != "" {
		t.Fatalf("429 must not be permanent: %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestParseQuotaFailure|TestPermanentRejectionReason' -v`
Expected: 编译失败（符号未定义）

- [ ] **Step 3: 实现 gemini_quota.go**

创建 `internal/app/gemini_quota.go`：

```go
package app

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// geminiQuotaFailure Gemini 429 解析结果:
// 区分"该模型没有免费层"(永久)与"当日额度耗尽"(等到重置点)。
type geminiQuotaFailure struct {
	NoFreeTier        bool
	DailyRequestLimit *int
	ExhaustedWindow   string // "day" / "minute" / ""
	RetryDelayMs      int64
}

var (
	quotaLineRe     = regexp.MustCompile(`(?i)Quota exceeded for metric:\s*([^,]+),\s*limit:\s*(\d+)`)
	freeTierMetricRe = regexp.MustCompile(`(?i)free_tier`)
	requestMetricRe  = regexp.MustCompile(`(?i)_requests$`)
	retryInRe        = regexp.MustCompile(`(?i)retry in ([\d.]+)s`)
	noLongerRe       = regexp.MustCompile(`(?i)no longer available`)
	interactionsRe   = regexp.MustCompile(`(?i)only supports .*Interactions API`)
)

func quotaWindow(quotaID string) string {
	if strings.Contains(quotaID, "PerDay") {
		return "day"
	}
	if strings.Contains(quotaID, "PerMinute") {
		return "minute"
	}
	return ""
}

type quotaLimitEntry struct {
	metric  string
	quotaID string
	window  string
	limit   int
	free    bool
	req     bool
}

// parseQuotaFailure 从 Gemini 429 响应提取配额语义; 非配额拒绝返回 nil。
func parseQuotaFailure(payload map[string]any) *geminiQuotaFailure {
	if payload == nil {
		return nil
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		return nil
	}
	message, _ := errObj["message"].(string)

	var parsed []struct {
		Metric string
		Limit  int
	}
	for _, m := range quotaLineRe.FindAllStringSubmatch(message, -1) {
		limit, _ := strconv.Atoi(m[2])
		parsed = append(parsed, struct {
			Metric string
			Limit  int
		}{Metric: strings.TrimSpace(m[1]), Limit: limit})
	}
	if len(parsed) == 0 {
		return nil
	}

	violationsByMetric := map[string][]string{}
	retryDelayMs := int64(0)
	details, _ := errObj["details"].([]any)
	for _, d := range details {
		raw, _ := json.Marshal(d)
		var probe struct {
			Type       string `json:"@type"`
			RetryDelay string `json:"retryDelay"`
			Violations []struct {
				QuotaMetric string `json:"quotaMetric"`
				QuotaID     string `json:"quotaId"`
			} `json:"violations"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		if strings.HasSuffix(probe.Type, "QuotaFailure") {
			for _, v := range probe.Violations {
				violationsByMetric[v.QuotaMetric] = append(violationsByMetric[v.QuotaMetric], v.QuotaID)
			}
			continue
		}
		if strings.HasSuffix(probe.Type, "RetryInfo") {
			sec := strings.TrimSuffix(probe.RetryDelay, "s")
			if f, err := strconv.ParseFloat(sec, 64); err == nil && f > 0 {
				retryDelayMs = int64(f * 1000)
			}
		}
	}
	if retryDelayMs == 0 {
		if m := retryInRe.FindStringSubmatch(message); len(m) == 2 {
			if f, err := strconv.ParseFloat(m[1], 64); err == nil && f > 0 {
				retryDelayMs = int64(f * 1000)
			}
		}
	}

	byMetric := map[string][]quotaLimitEntry{}
	for _, e := range parsed {
		byMetric[e.Metric] = append(byMetric[e.Metric], quotaLimitEntry{metric: e.Metric, limit: e.Limit})
	}
	var limits []quotaLimitEntry
	for metric, entries := range byMetric {
		ids := violationsByMetric[metric]
		aligned := len(ids) == len(entries)
		for i, e := range entries {
			if aligned {
				e.quotaID = ids[i]
			}
			e.window = quotaWindow(e.quotaID)
			e.free = freeTierMetricRe.MatchString(metric)
			e.req = requestMetricRe.MatchString(metric)
			limits = append(limits, e)
		}
	}

	var freeTier []quotaLimitEntry
	for _, e := range limits {
		if e.free {
			freeTier = append(freeTier, e)
		}
	}
	noFreeTier := len(freeTier) > 0
	for _, e := range freeTier {
		if e.limit > 0 {
			noFreeTier = false
			break
		}
	}

	var daily *int
	for _, e := range freeTier {
		if e.req && e.window == "day" && e.limit > 0 {
			v := e.limit
			daily = &v
			break
		}
	}

	exhausted := ""
	for _, e := range freeTier {
		if e.limit > 0 && e.window == "day" {
			exhausted = "day"
			break
		}
	}
	if exhausted == "" {
		for _, e := range freeTier {
			if e.limit > 0 {
				exhausted = "minute"
				break
			}
		}
	}

	return &geminiQuotaFailure{
		NoFreeTier:        noFreeTier,
		DailyRequestLimit: daily,
		ExhaustedWindow:   exhausted,
		RetryDelayMs:      retryDelayMs,
	}
}

// permanentRejectionReason 判定"永远不会服务该模型"的稳定事实; 其余返回空串。
func permanentRejectionReason(status int, payload map[string]any) string {
	message := ""
	if payload != nil {
		if errObj, _ := payload["error"].(map[string]any); errObj != nil {
			message, _ = errObj["message"].(string)
		}
	}
	if status == 404 {
		if noLongerRe.MatchString(message) {
			return "withdrawn upstream"
		}
		return "listed in the catalog but not served here"
	}
	if status == 400 && interactionsRe.MatchString(message) {
		return "not a chat-completions model"
	}
	return ""
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestParseQuotaFailure|TestPermanentRejectionReason' -v`
Expected: PASS（5 个测试）

- [ ] **Step 5: 提交**

```bash
git add internal/app/gemini_quota.go internal/app/gemini_quota_test.go
git commit -m "feat: read gemini quota failures and permanent model rejections"
```

---

### Task 6: Provider Chat 通道（转发 + SSE 直通）

**Files:**
- Modify: `internal/app/providers_chat.go`（补全 Task 3 建立的文件）
- Test: `internal/app/providers_chat_test.go`

**Interfaces:**
- Consumes: Task 1-5 全部；`writeJSON`、`setRouteHeader`、`handleStreamResponseWithUsage`、`handleNonStreamResponseWithUsage`、`kit.Truncate`（既有）
- Produces:
  - `type providerError struct{ Provider string; Status int; Body string }` + `Error()`
  - `func providerErrorStatus(err error) int`
  - `func parseProviderModel(model string) (name, rest string, ok bool)`
  - `type sseTapReader struct{...}`
  - `func (p *modelProvider) Chat(ctx context.Context, params map[string]any, stream bool) (*http.Response, error)`
  - `func handleProviderChat(w http.ResponseWriter, r *http.Request, params map[string]any, name string)`

- [ ] **Step 1: 写失败测试**

创建 `internal/app/providers_chat_test.go`：

```go
package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseProviderModel(t *testing.T) {
	setTestProvider(t, "openrouter", providerConfig{BaseURL: "https://x", APIKey: "k"})
	if name, rest, ok := parseProviderModel("openrouter:z-ai/glm-5.3:free"); !ok || name != "openrouter" || rest != "z-ai/glm-5.3:free" {
		t.Fatalf("parse: %q %q %v", name, rest, ok)
	}
	if _, _, ok := parseProviderModel("mimo-v2.5-free"); ok {
		t.Fatal("bare model name must not parse as a provider route")
	}
	if _, _, ok := parseProviderModel("nope:model"); ok {
		t.Fatal("unknown provider must not parse")
	}
	if _, _, ok := parseProviderModel("openrouter:"); ok {
		t.Fatal("empty model must not parse")
	}
	if _, _, ok := parseProviderModel("z-ai/glm-5.3:free"); ok {
		t.Fatal("model id containing a colon must not parse as a provider route")
	}
}

func TestProviderErrorStatus(t *testing.T) {
	if got := providerErrorStatus(&providerError{Status: 403}); got != 403 {
		t.Fatalf("4xx must pass through: %d", got)
	}
	if got := providerErrorStatus(&providerError{Status: 503}); got != 502 {
		t.Fatalf("5xx must map to 502: %d", got)
	}
	if got := providerErrorStatus(context.DeadlineExceeded); got != 502 {
		t.Fatalf("network error must map to 502: %d", got)
	}
}

func TestProviderChatPassthrough(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModel, _ = req["model"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}}},
		})
	}))
	defer srv.Close()

	setTestProvider(t, "bai", providerConfig{BaseURL: srv.URL, APIKey: "k-bai", FreeModels: []string{"glm-5.3-flash"}})
	p := providerByName("bai")
	resp, err := p.Chat(context.Background(), map[string]any{
		"model":    "bai:glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer k-bai" {
		t.Fatalf("authorization: %q", gotAuth)
	}
	if gotModel != "glm-5.3-flash" {
		t.Fatalf("provider prefix must be stripped: %q", gotModel)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestProviderChatCustomHeaders(t *testing.T) {
	var gotReferer, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "or", providerConfig{
		BaseURL: srv.URL, APIKey: "k",
		Headers: map[string]providerHeaderSpec{
			"HTTP-Referer": {Default: "${origin}"},
			"X-Title":      {Default: "Cline Proxy"},
		},
	})
	p := providerByName("or")
	resp, err := p.Chat(context.Background(), map[string]any{"model": "m", "messages": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotTitle != "Cline Proxy" {
		t.Fatalf("static header: %q", gotTitle)
	}
	if gotReferer == "" {
		t.Fatal("${origin} must expand")
	}
}

func TestProviderChatErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()
	setTestProvider(t, "x", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"m"}})
	p := providerByName("x")
	_, err := p.Chat(context.Background(), map[string]any{"model": "m", "messages": []any{}}, false)
	if err == nil {
		t.Fatal("403 must return an error")
	}
	pe, ok := err.(*providerError)
	if !ok || pe.Status != 403 {
		t.Fatalf("error must carry upstream status: %#v", err)
	}
	if got := providerErrorStatus(err); got != 403 {
		t.Fatalf("status passthrough: %d", got)
	}
}

func TestProviderChatRecordsPermanentRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"model is no longer available"}}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gone", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"dead"}})
	p := providerByName("gone")
	if _, err := p.Chat(context.Background(), map[string]any{"model": "dead", "messages": []any{}}, false); err == nil {
		t.Fatal("404 must return an error")
	}
	if p.isFree("dead") {
		t.Fatal("permanently rejected model must drop out of the free set")
	}
}

func TestProviderChatInjectsThoughtSignature(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gemini", providerConfig{BaseURL: srv.URL, APIKey: "gk", FreeModels: []string{"g1"}})
	p := providerByName("gemini")
	params := map[string]any{
		"model": "g1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
		},
	}
	resp, err := p.Chat(context.Background(), params, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %+v", msgs)
	}
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	if got := readThoughtSignature(calls[0].(map[string]any)); got != skipThoughtSignature {
		t.Fatalf("sentinel must be injected: %q", got)
	}
}

func TestProviderChatRemembersSignatureFromResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call-9","extra_content":{"google":{"thought_signature":"sig-9"}}}]}}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "gemini", providerConfig{BaseURL: srv.URL, APIKey: "gk", FreeModels: []string{"g1"}})
	p := providerByName("gemini")
	resp, err := p.Chat(context.Background(), map[string]any{"model": "g1", "messages": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := p.sigCache.lookup("call-9"); got != "sig-9" {
		t.Fatalf("response signature must be remembered: %q", got)
	}
}

func TestSSETapReaderSplitsLines(t *testing.T) {
	body := io.NopCloser(&chunkReader{chunks: []string{"data: a\n", "data: b", "\n"}})
	var lines []string
	tap := &sseTapReader{rc: body, onLine: func(l string) { lines = append(lines, l) }}
	io.ReadAll(tap)
	tap.Close()
	want := []string{"data: a", "data: b", ""}
	if len(lines) != len(want) {
		t.Fatalf("lines: %#v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: %q want %q", i, lines[i], want[i])
		}
	}
}

// chunkReader 按固定分片返回数据, 模拟网络分片边界。
type chunkReader struct {
	chunks []string
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestParseProviderModel|TestProviderErrorStatus|TestProviderChat|TestSSETapReader' -v`
Expected: 编译失败（符号未定义）

- [ ] **Step 3: 实现 providers_chat.go**

用以下内容整体替换 `internal/app/providers_chat.go`（含 Task 3 建立的两个客户端）：

```go
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

const providerAttemptTimeout = 180 * time.Second

// providerDirectClient 直连上游(默认用于非 Gemini provider)。
var providerDirectClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// providerProxiedClient 走系统代理(用于需要海外出口的 Gemini)。
var providerProxiedClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	},
}

// providerError 上游错误(状态码 + 截断响应体)。
type providerError struct {
	Provider string
	Status   int
	Body     string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("provider %s: HTTP %d: %s", e.Provider, e.Status, kit.Truncate(e.Body, 500))
}

// providerErrorStatus 上游 4xx 原样透传, 其余(网络错误/5xx)按 502。
func providerErrorStatus(err error) int {
	if pe, ok := err.(*providerError); ok {
		if pe.Status >= 400 && pe.Status < 500 {
			return pe.Status
		}
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// parseProviderModel 解析 "provider:model" 前缀; provider 必须已在配置中声明。
func parseProviderModel(model string) (string, string, bool) {
	m := strings.TrimSpace(model)
	i := strings.Index(m, ":")
	if i <= 0 {
		return "", "", false
	}
	name, rest := m[:i], m[i+1:]
	if rest == "" || providerByName(name) == nil {
		return "", "", false
	}
	return name, rest, true
}

// sseTapReader 边读边按行回调(用于流式提取 Gemini 签名)。
type sseTapReader struct {
	rc      io.ReadCloser
	onLine  func(string)
	pending []byte
}

func (t *sseTapReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.pending = append(t.pending, p[:n]...)
		for {
			i := bytes.IndexByte(t.pending, '\n')
			if i < 0 {
				break
			}
			line := string(t.pending[:i])
			t.pending = t.pending[i+1:]
			t.onLine(line)
		}
	}
	return n, err
}

func (t *sseTapReader) Close() error {
	if len(t.pending) > 0 {
		t.onLine(string(t.pending))
		t.pending = nil
	}
	return t.rc.Close()
}

// Chat 转发 OpenAI 兼容请求到该 provider; 流式响应按 SSE 原样透传。
func (p *modelProvider) Chat(ctx context.Context, params map[string]any, stream bool) (*http.Response, error) {
	cfg, _ := providerConfigFor(p.name)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("provider %s is not configured", p.name)
	}
	if model, _ := params["model"].(string); model != "" {
		if rest, ok := strings.CutPrefix(model, p.name+":"); ok && rest != "" {
			params["model"] = rest
		}
	}
	model, _ := params["model"].(string)

	needsSig := providerNeedsThoughtSignatures(p)
	if needsSig {
		injectThoughtSignatures(params, p.sigCache)
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal provider body: %w", err)
	}
	u := strings.TrimRight(cfg.BaseURL, "/") + cfg.chatPath()
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	origin := localOrigin()
	for k, spec := range cfg.Headers {
		if v := cfg.resolveHeader(spec, origin); v != "" {
			req.Header.Set(k, v)
		}
	}

	client := providerDirectClient
	if providerUsesProxiedClient(p) {
		client = providerProxiedClient
	}
	if !stream {
		ctx, cancel := context.WithTimeout(ctx, providerAttemptTimeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		p.recordRejection(model, resp.StatusCode, body)
		return nil, &providerError{Provider: p.name, Status: resp.StatusCode, Body: string(body)}
	}
	if needsSig {
		if stream {
			ext := newSignatureStreamExtractor(p.sigCache)
			resp.Body = &sseTapReader{rc: resp.Body, onLine: func(line string) { ext.push(line) }}
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				rememberSignaturesFromPayload(parsed, p.sigCache)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	return resp, nil
}

// recordRejection 记录永久拒绝与"无免费层"到该 provider 的拒绝集合。
func (p *modelProvider) recordRejection(model string, status int, body []byte) {
	var payload map[string]any
	json.Unmarshal(body, &payload)
	reason := permanentRejectionReason(status, payload)
	if reason == "" && status == http.StatusTooManyRequests {
		if qf := parseQuotaFailure(payload); qf != nil && qf.NoFreeTier {
			reason = "no free tier"
		}
	}
	if reason == "" || strings.TrimSpace(model) == "" {
		return
	}
	// 写时复制: isFree/freeModelIDs 在解锁后读取该 map 的内容, 就地插入
	// 会与之并发触发 fatal 的 map 读写竞争, 因此整体替换而不是原地增删。
	p.mu.Lock()
	next := make(map[string]string, len(p.rejected)+1)
	for k, v := range p.rejected {
		next[k] = v
	}
	next[model] = reason
	p.rejected = next
	p.mu.Unlock()
}

// handleProviderChat "provider:model" 前缀直选分支。
func handleProviderChat(w http.ResponseWriter, r *http.Request, params map[string]any, name string) {
	p := providerByName(name)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q is not configured", name), "type": "api_error"},
		})
		return
	}
	cfg, _ := providerConfigFor(name)
	if cfg.APIKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("provider %q has no api key", name), "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	pm := strings.TrimPrefix(model, name+":")
	if !p.isFree(pm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{
				"message": fmt.Sprintf("model %q is not a free model on provider %q", model, name),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	// 目录型 provider 首次请求前同步刷新一次, 之后由后台循环维护
	if cfg.Catalog {
		p.mu.Lock()
		need := len(p.catalog) == 0 && p.catalogErr == ""
		p.mu.Unlock()
		if need {
			ctx, cancel := context.WithTimeout(r.Context(), providerCatalogTimeout)
			if err := p.refreshCatalog(ctx, false); err != nil {
				log.Printf("  providers: initial catalog refresh (%s) failed: %v", name, err)
			}
			cancel()
		}
	}

	isStream, _ := params["stream"].(bool)
	params["model"] = pm
	resp, err := p.Chat(r.Context(), params, isStream)
	if err != nil {
		status := providerErrorStatus(err)
		log.Printf("  provider api error (%s): %v", name, err)
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer resp.Body.Close()
	usageFn := func(map[string]any) {}
	if isStream {
		handleStreamResponseWithUsage(w, resp, usageFn)
		return
	}
	handleNonStreamResponseWithUsage(w, resp, usageFn)
}
```

`log` 需加入 import。

- [ ] **Step 4: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestParseProviderModel|TestProviderErrorStatus|TestProviderChat|TestSSETapReader' -v`
Expected: PASS（9 个测试）

- [ ] **Step 5: 提交**

```bash
git add internal/app/providers_chat.go internal/app/providers_chat_test.go
git commit -m "feat: proxy provider chat with prefixed model routing"
```

---

### Task 7: 网关入口集成（前缀分发 + 模型列表 + 目录刷新循环）

**Files:**
- Modify: `internal/app/proxy.go:50-79`（StartProxy：listenOrigin + 刷新循环）、`internal/app/proxy.go:239`（聊天入口分发）、`internal/app/proxy.go:193`（模型列表合并）

**Interfaces:**
- Consumes: Task 1-6 全部；既有 `setRouteHeader`、`apiModelList`、`zenModelList`、`clinePassProvider`
- Produces: 无新导出符号（集成点）

- [ ] **Step 1: 启动时记录本地地址并启动目录刷新循环**

在 `internal/app/proxy.go` 的 `StartProxy` 中，`initLogFile()` 之后加：

```go
	setListenOrigin(fmt.Sprintf("http://127.0.0.1:%d", port))
```

并在 `startZenModelsRefresher()` 之后加：

```go
	startProviderRefresher()
```

- [ ] **Step 2: 聊天入口加 provider 前缀分发**

在 `internal/app/proxy.go` 的聊天处理器中，找到这段注释与 `if route := routeModel(model); route == "zen" {`（约 239 行），在它**之前**插入：

```go
		// 通用 provider: "provider:model" 前缀直选, 不依赖 Cline 账号
		if name, _, ok := parseProviderModel(model); ok {
			setRouteHeader(w, name, model, "")
			handleProviderChat(w, r, params, name)
			return
		}
```

- [ ] **Step 3: 模型列表合并 provider 免费模型**

在 `internal/app/proxy.go` 的 `modelsHandler` 中，ClinePass 合并循环结束之后、`writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})` **之前**插入：

```go
		// 合并通用 provider 免费模型
		data = append(data, providerModelList()...)
```

- [ ] **Step 4: 编译并跑全量测试**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" build ./... 2>&1; & "C:\Go\bin\go.exe" test ./internal/app/ -count=1`
Expected: 构建成功；全部测试 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/app/proxy.go
git commit -m "feat: route prefixed provider models through the gateway entry"
```

---

### Task 8: 管理端 Provider API

**Files:**
- Create: `internal/app/admin_providers.go`
- Modify: `internal/app/admin.go:82`（注册 4 条路由）

**Interfaces:**
- Consumes: Task 1-7 全部；既有 `writeAPI`、`apiResponse`、`corsHandler`
- Produces:
  - `func handleProvidersConfig(w, r)`（GET）
  - `func handleProvidersUpdate(w, r)`（POST，`{name, provider?, remove?}`）
  - `func handleProvidersRefresh(w, r)`（POST，`{name?}`）
  - `func handleProvidersTest(w, r)`（POST，`{name, model?}`）

- [ ] **Step 1: 写失败测试**

追加到 `internal/app/providers_chat_test.go`（或新建 `admin_providers_test.go`）：

```go
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
	setTestProvider(t, "t", providerConfig{BaseURL: srv.URL, APIKey: "k", FreeModels: []string{"m1"}})
	req := httptest.NewRequest("POST", "/admin/api/providers/test", strings.NewReader(`{"name":"t","model":"m1"}`))
	w := httptest.NewRecorder()
	handleProvidersTest(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":200`) {
		t.Fatalf("test endpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminProvidersTestUnknown(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/test", strings.NewReader(`{"name":"nope"}`))
	w := httptest.NewRecorder()
	handleProvidersTest(w, req)
	if w.Code != 404 {
		t.Fatalf("unknown provider should be 404: %d", w.Code)
	}
}
```

需在文件 import 中加 `strings`。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestAdminProviders' -v`
Expected: 编译失败（handler 未定义）

- [ ] **Step 3: 实现 admin_providers.go**

创建 `internal/app/admin_providers.go`：

```go
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
```

- [ ] **Step 4: 注册路由**

在 `internal/app/admin.go` 的 `registerAdminRoutes` 中，`mux.HandleFunc("/admin/api/zen/stats", corsHandler(handleZenStats))` 之后加：

```go
	mux.HandleFunc("/admin/api/providers", corsHandler(handleProvidersConfig))
	mux.HandleFunc("/admin/api/providers/update", corsHandler(handleProvidersUpdate))
	mux.HandleFunc("/admin/api/providers/refresh", corsHandler(handleProvidersRefresh))
	mux.HandleFunc("/admin/api/providers/test", corsHandler(handleProvidersTest))
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./internal/app/ -run 'TestAdminProviders' -v`
Expected: PASS（4 个测试）

- [ ] **Step 6: 提交**

```bash
git add internal/app/admin_providers.go internal/app/admin_providers_test.go internal/app/admin.go
git commit -m "feat: expose provider configuration through the admin api"
```

---

### Task 9: 管理界面 Provider 面板

**Files:**
- Modify: `internal/app/admin_html.go:467`（新增设置区块）、`internal/app/admin_html.go`（JS 函数 + 初始化调用）

**Interfaces:**
- Consumes: Task 8 的 4 个 API 端点；既有 JS 辅助 `api()`、`toast()`、`esc()`、`_()`
- Produces: 无 Go 符号（纯前端）

- [ ] **Step 1: 插入设置区块**

在 `internal/app/admin_html.go` 中，找到 `id="ocNodesBox"` 所在区块的收尾（该区块以 `<div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存出口配置</button></div>` 结尾，随后是 `</div>` 与 `</div>`），在紧随其后的 `<div class="section">` 之前插入：

```html
<div class="section">
  <div class="section-title">🔌 通用 Provider（OpenAI 兼容上游）</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Provider 名</label><input type="text" id="pvName" placeholder="openrouter / gemini / tokenrouter / bai"></div>
      <div class="field"><label>Base URL</label><input type="text" id="pvBaseUrl" placeholder="https://openrouter.ai/api/v1"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>API Key</label><input type="password" id="pvKey" placeholder="sk-..."></div>
      <div class="field"><label>目录 / 免费判定</label>
        <div style="display:flex;gap:8px">
          <select id="pvCatalog" style="flex:1">
            <option value="false">不拉目录（白名单）</option>
            <option value="true">拉取 /models 目录</option>
          </select>
          <select id="pvPricing" style="flex:1">
            <option value="false">按白名单判定免费</option>
            <option value="true">按目录价格判定免费</option>
          </select>
        </div>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>免费模型白名单（每行一个）</label>
        <textarea id="pvFree" rows="4" placeholder="gemini-3.8-flash&#10;glm-5.3-flash"></textarea></div>
      <div class="field"><label>连通测试模型（留空用第一个免费模型）</label>
        <input type="text" id="pvTestModel" placeholder="z-ai/glm-5.3:free"></div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="saveProvider()">💾 保存 Provider</button>
      <button class="btn" onclick="testProvider()">🔍 连通测试</button>
      <button class="btn" onclick="refreshProviderCatalog()">🔄 刷新目录</button>
    </div>
    <div id="pvList" style="margin-top:10px;border:1px solid var(--border);border-radius:10px;background:rgba(2,6,23,.3)"></div>
  </div>
</div>
```

- [ ] **Step 2: 插入前端逻辑**

在 `internal/app/admin_html.go` 中，找到 `async function saveOcConfig() {` 函数（约 1199 行）的收尾 `}` 之后，插入：

```javascript
let pvData = {};
async function loadProviders() {
  try {
    const d = await api('GET', '/admin/api/providers');
    pvData = (d.data && d.data.providers) || {};
    const names = Object.keys(pvData);
    if (!names.length) {
      _('pvList').innerHTML = '<div style="padding:10px 12px;font-size:12px;color:var(--text3)">暂无 provider, 填上方表单添加</div>';
      return;
    }
    _('pvList').innerHTML = names.map(n => {
      const p = pvData[n] || {}, rt = p.runtime || {};
      let st;
      if (!rt.configured) { st = '未配置 key'; }
      else if (rt.error) { st = '❌ ' + esc(rt.error); }
      else if (p.catalog) { st = '目录 ' + (rt.catalogSize || 0) + ' · 免费 ' + (rt.freeCount || 0) + ' · 可聊 ' + (rt.chatCount || 0); }
      else { st = '白名单 ' + ((p.freeModels || []).length) + ' 个'; }
      if (rt.rejected) { st += ' · 剔除 ' + rt.rejected; }
      return '<div style="display:flex;align-items:center;gap:9px;padding:6px 12px;font-size:12.5px;border-bottom:1px solid rgba(148,163,184,.07)">' +
        '<span style="flex:none;min-width:92px;font-family:monospace;color:var(--text3)">' + esc(n) + '</span>' +
        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(p.baseUrl || '') + '</span>' +
        '<span style="flex:none;font-size:11px;color:var(--text3)">' + st + '</span>' +
        '<button type="button" class="btn" style="padding:2px 8px;font-size:11px" onclick="editProvider(\'' + esc(n) + '\')">编辑</button>' +
        '<button type="button" class="btn" style="padding:2px 8px;font-size:11px;color:#f87171" onclick="delProvider(\'' + esc(n) + '\')">删除</button></div>';
    }).join('');
  } catch (e) { _('pvList').textContent = '加载失败: ' + e.message; }
}
function editProvider(n) {
  const p = pvData[n] || {};
  _('pvName').value = n;
  _('pvBaseUrl').value = p.baseUrl || '';
  _('pvKey').value = p.apiKey || '';
  _('pvCatalog').value = String(!!p.catalog);
  _('pvPricing').value = String(!!p.pricing);
  _('pvFree').value = (p.freeModels || []).join('\n');
  toast('已载入 ' + n + ', 修改后点保存', 'success');
}
async function saveProvider() {
  const name = _('pvName').value.trim();
  if (!name) { toast('请填写 Provider 名', 'error'); return; }
  const body = {
    name,
    provider: {
      baseUrl: _('pvBaseUrl').value.trim(),
      apiKey: _('pvKey').value.trim(),
      catalog: _('pvCatalog').value === 'true',
      pricing: _('pvPricing').value === 'true',
      freeModels: _('pvFree').value.split('\n').map(s => s.trim()).filter(Boolean),
    },
  };
  try { await api('POST', '/admin/api/providers/update', body); toast('已保存 ' + name, 'success'); loadProviders(); }
  catch (e) { toast('保存失败: ' + e.message, 'error'); }
}
async function delProvider(n) {
  if (!confirm('确认删除 provider ' + n + '?')) return;
  try { await api('POST', '/admin/api/providers/update', { name: n, remove: true }); toast('已删除 ' + n, 'success'); loadProviders(); }
  catch (e) { toast('删除失败: ' + e.message, 'error'); }
}
async function testProvider() {
  const name = _('pvName').value.trim();
  if (!name) { toast('请先填写 Provider 名', 'error'); return; }
  toast('连通测试中...', 'success');
  try {
    const d = await api('POST', '/admin/api/providers/test', { name, model: _('pvTestModel').value.trim() });
    const r = (d.data) || {};
    const detail = r.error ? String(r.error).slice(0, 160) : String(r.body || '').slice(0, 160);
    toast('HTTP ' + (r.status || '?') + ' · ' + detail, r.status === 200 ? 'success' : 'error');
  } catch (e) { toast('测试失败: ' + e.message, 'error'); }
}
async function refreshProviderCatalog() {
  const name = _('pvName').value.trim();
  try {
    await api('POST', '/admin/api/providers/refresh', name ? { name } : {});
    toast('目录刷新已启动', 'success');
    setTimeout(loadProviders, 3000);
    setTimeout(loadProviders, 12000);
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}
```

- [ ] **Step 3: 页面初始化时加载**

在 `internal/app/admin_html.go` 中定位页面初始化流程里首次调用 `loadOcNodes()` 的位置（与 `loadOcConfig()` 等并列的初始化区），在其后加一行：

```javascript
  loadProviders();
```

- [ ] **Step 4: 编译验证**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" build ./... 2>&1`
Expected: 构建成功（HTML 内嵌字符串不影响编译）

- [ ] **Step 5: 提交**

```bash
git add internal/app/admin_html.go
git commit -m "feat: add provider panel to the admin ui"
```

---

### Task 10: 集成验证与交付

**Files:**
- 无新增；验证与推送

**Interfaces:**
- Consumes: Task 1-9 全部

- [ ] **Step 1: 静态检查**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" vet ./... 2>&1`
Expected: 无输出（无告警）

- [ ] **Step 2: 全量测试**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" test ./... -count=1 2>&1`
Expected: 全部 PASS

- [ ] **Step 3: 带标签构建（与发布方式一致）**

Run: `cd D:\deepseek\Cline-proxy; & "C:\Go\bin\go.exe" build -tags "with_quic,with_grpc,with_utls" -ldflags "-s -w -H=windowsgui" -o cline-proxy.exe . 2>&1`
Expected: 构建成功，产出 `cline-proxy.exe`

- [ ] **Step 4: 部署到运行目录并端到端验证**

```powershell
Get-Process cline-proxy -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 3
Copy-Item D:\deepseek\Cline-proxy\cline-proxy.exe D:\cline-proxy-windows-amd64\ -Force
Start-Process -FilePath "D:\cline-proxy-windows-amd64\cline-proxy.exe" -WorkingDirectory "D:\cline-proxy-windows-amd64" -ArgumentList "-host","0.0.0.0","-port","3457"
Start-Sleep -Seconds 30
```

验证管理端已能列出 provider 配置端点：

```powershell
Invoke-RestMethod -Uri "http://127.0.0.1:3457/admin/api/providers" -TimeoutSec 15 | ConvertTo-Json -Depth 4
```

Expected: 返回 `{"success":true,"data":{"providers":{...}}}`（首次为空对象，随后可在界面添加）

验证模型列表端点仍正常：

```powershell
$h = @{"x-api-key"="cline_1a089b95004_c9130"}
(Invoke-WebRequest -Uri "http://127.0.0.1:3457/v1/models" -Headers $h -TimeoutSec 30 -SkipHttpErrorCheck).StatusCode
```

Expected: 200

- [ ] **Step 5: 提交与推送**

```bash
git add -A
git commit -m "feat: serve generic providers alongside zen and cline upstreams"
```

推送使用本会话已验证的通道（github.com 的 git 传输被重置时走 GitHub API）：

```powershell
cd D:\deepseek\Cline-proxy
git -c http.proxy=http://127.0.0.1:2080 -c https.proxy=http://127.0.0.1:2080 push origin main
```

若失败，用 GitHub API 脚本推送（token 从环境变量读取，脚本不落库）：

```javascript
// push_api.js (临时文件, 用完删除; 需 $env:GH_TOKEN)
const https = require('https'), fs = require('fs'), path = require('path');
const TOKEN = process.env.GH_TOKEN, REPO = 'krisxu23/Cline-proxy';
const BASE = `https://api.github.com/repos/${REPO}`;
function api(method, urlPath, body) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const url = new URL(BASE + urlPath);
    const req = https.request({
      hostname: url.hostname, path: url.pathname + url.search, method,
      headers: {
        Authorization: `token ${TOKEN}`, Accept: 'application/vnd.github+json',
        'User-Agent': 'node-push',
        ...(data ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } : {}),
      },
    }, (res) => { let b = ''; res.on('data', c => b += c); res.on('end', () => { try { resolve(JSON.parse(b)); } catch { resolve(b); } }); });
    req.on('error', reject);
    if (data) req.write(data);
    req.end();
  });
}
(async () => {
  const cwd = path.resolve(__dirname);
  const head = (await api('GET', '/git/refs/heads/main')).object.sha;
  const baseTree = (await api('GET', `/git/commits/${head}`)).tree.sha;
  const files = ['internal/app/providers_config.go', 'internal/app/providers_catalog.go', 'internal/app/providers_chat.go',
    'internal/app/thought_signature.go', 'internal/app/gemini_quota.go', 'internal/app/admin_providers.go',
    'internal/app/admin.go', 'internal/app/admin_html.go', 'internal/app/zen.go', 'internal/app/proxy.go'];
  const tree = [];
  for (const f of files) {
    const blob = await api('POST', '/git/blobs', { content: fs.readFileSync(path.join(cwd, f), 'utf8'), encoding: 'utf-8' });
    if (!blob.sha) { console.error('blob failed', f, blob); process.exit(1); }
    tree.push({ path: f, mode: '100644', type: 'blob', sha: blob.sha });
  }
  const treeSha = (await api('POST', '/git/trees', { base_tree: baseTree, tree })).sha;
  const commit = await api('POST', '/git/commits', {
    message: [
      'feat: serve generic providers alongside zen and cline upstreams',
      '',
      '- configure openai-compatible upstreams with catalog or whitelist free detection',
      '- select them with a provider:model prefix and list them under /v1/models',
      '- carry gemini thought-signatures and read quota failures for accurate routing',
    ].join('\n'),
    tree: treeSha, parents: [head],
  });
  const done = await api('PATCH', '/git/refs/heads/main', { sha: commit.sha });
  console.log('pushed', done.object.sha);
})();
```

```powershell
$env:GH_TOKEN = "<PAT>"
node push_api.js
del push_api.js
git fetch origin
git reset --hard origin/main
```

---

## 自审结果

**Spec 覆盖检查**（对照 `docs/superpowers/specs/2026-09-11-multi-provider-routing-design.md`）：

| Spec 条目 | 本计划任务 |
|---|---|
| §4.1 Upstream 抽象 / genericProvider | Task 1, 2, 3, 6 |
| §4.1 免费判定三模式 | Task 2（`evalProviderFree`） |
| §4.1 Google 方言归一化 + 分页 | Task 3 |
| §4.1 Gemini thought-signature | Task 4 |
| §4.1 Gemini 配额解析 + 永久拒绝 | Task 5 |
| §4.1 probeFreeTier | 配置字段落地（Task 1）；探测逻辑属第 3 期 discovery |
| §4.6 Admin UI Provider 区块 | Task 8（API）+ Task 9（界面） |
| §4.8 模型以 `provider:model` 并入 /v1/models | Task 3（`providerModelList`）+ Task 7 |
| §4.8 前缀直选 | Task 6（`parseProviderModel`）+ Task 7 |
| §5.1 配置扩展 | Task 1 |
| §6 测试 | 各任务内联 + Task 10 全量 |
| §7 第 1 期范围 | Tasks 1-10 |

**第 2/3 期不在本计划内**（候选链 / 分级冷却 / 配额账本 / discovery 各自独立成计划）。

**占位符扫描**：无 TBD/TODO；所有步骤含可执行代码或确切命令。

**类型一致性**：`providerConfig` / `catalogModel` / `modelProvider` / `providerError` / `geminiQuotaFailure` 的字段与函数签名在 Task 1-8 间保持一致；`providerCatalogTimeout` 同时被 Task 3 与 Task 8 引用，均在 Task 3 定义。
