package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 自动路由页面的接口。
//
// 页面形态: 网关自己探测出"当前挂了哪些供应商"，全部罗列出来让用户勾选；
// 勾中的供应商再把名下模型列出来，由用户勾选哪些模型参与自动路由。
// 因此后端不再要求用户手填供应商名 —— 探测来源就是 providers 配置本身。

// routerAliasRe 自动路由模型名的合法形态。
// 不允许空白与冒号: 冒号在上游标识里是 "provider:model" 的分隔符,
// 别名里带上它会让解析产生歧义。
var routerAliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// routerSnapshot 组装页面需要的全部数据。
//
// 供应商树 = 内置上游(opencode zen / cline 账号池) + 通用 Provider。
// 候选链表只展示配置里真实存在的 routes 条目 —— 没保存过就是空白,
// 不再合成"默认链"或历史别名, 避免出现用户没配过却看到内容的情况。
func routerSnapshot() map[string]any {
	alias := autoRouterAlias()
	cfg := getZenConfig()

	selectedModels := map[string]bool{}
	configuredProviders := map[string]bool{}
	freshInstall := true // 从未保存过任何勾选
	if cfg != nil {
		for _, e := range cfg.Routes[alias] {
			if e = strings.TrimSpace(e); e != "" {
				selectedModels[e] = true
			}
		}
		for _, p := range cfg.Router.Providers {
			if p = strings.TrimSpace(p); p != "" {
				configuredProviders[p] = true
			}
		}
		freshInstall = len(selectedModels) == 0 && len(configuredProviders) == 0
	}

	problems := []string{}
	selectedCount := 0
	providers := make([]map[string]any, 0, 8)

	// addProvider 把一个上游(内置或通用 Provider)加进页面列表。
	// models 元素: {id, context, output, selected}。
	addProvider := func(name, display string, builtin, configured, google bool, models []map[string]any) {
		chosen := configuredProviders[name]
		if !chosen {
			for _, m := range models {
				if m["selected"].(bool) {
					chosen = true
					break
				}
			}
			// 从未配置过任何选择时按默认链如实回显:
			// zen 恒可用, cline 看账号池, 通用 Provider 看 API Key。
			if freshInstall && configured {
				chosen = true
			}
		}
		for _, m := range models {
			if m["selected"].(bool) {
				selectedCount++
			}
		}
		providers = append(providers, map[string]any{
			"name":       name,
			"display":    display,
			"builtin":    builtin,
			"configured": configured,
			"google":     google,
			"selected":   chosen,
			"models":     models,
		})
	}

	// 1) opencode(zen): seed 白名单保证至少有几个免费模型, 恒可用。
	zenModels := make([]map[string]any, 0, 8)
	for _, m := range zenFreeCatalog() {
		key := candidateKey(upstreamZen, m.ID)
		zenModels = append(zenModels, map[string]any{
			"id":       m.ID,
			"context":  m.Context,
			"output":   m.Output,
			"selected": selectedModels[key],
		})
	}
	addProvider(upstreamZen, "opencode（zen 免费模型）", true, true, false, zenModels)

	// 2) cline 账号池: 单个占位模型 "*", 具体模型由池内轮询决定。
	clineOK := clinePoolReady()
	if !clineOK {
		problems = append(problems, "Cline 账号池还没有可用账号，请先到「账号管理」导入账号")
	}
	clineModels := []map[string]any{{
		"id":       clinePoolPlaceholder,
		"context":  0,
		"output":   0,
		"selected": selectedModels[candidateKey(upstreamCline, clinePoolPlaceholder)],
	}}
	addProvider(upstreamCline, "Cline 账号池（自动选账号与模型）", true, clineOK, false, clineModels)

	// 3) 通用 Provider。
	for _, name := range providerNames() {
		pc, _ := providerConfigFor(name)
		p := providerByName(name)

		models := make([]map[string]any, 0, 8)
		for _, m := range freeModelsFor(name, p) {
			key := candidateKey(name, m.ID)
			models = append(models, map[string]any{
				"id":       m.ID,
				"context":  m.ContextLength,
				"output":   m.MaxOutput,
				"selected": selectedModels[key],
			})
		}

		configured := pc.APIKey != ""
		if !configured {
			problems = append(problems, fmt.Sprintf("供应商 %s 还没有填写 API Key，它的模型无法参与自动路由", name))
		} else if len(models) == 0 {
			problems = append(problems, fmt.Sprintf("供应商 %s 还没有拉取到可用模型，请在「设置 → 通用 Provider」点一次刷新目录", name))
		}

		addProvider(name, name, false, configured, isGoogleProvider(pc), models)
	}

	// 候选链表: 只列配置里真实存在的 routes 条目。
	routes := []map[string]any{}
	if cfg != nil {
		keys := make([]string, 0, len(cfg.Routes))
		for k, list := range cfg.Routes {
			if k != "" && len(list) > 0 {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			routes = append(routes, describeRouteChain(k))
		}
	}

	return map[string]any{
		"alias":        alias,
		"providers":    providers,
		"selected":     selectedCount,
		"problems":     problems,
		"routes":       routes,
		"cooling":      candidateCoolingSnapshot(),
		"permanent":    permanentRejectionList(),
		"usage":        usageSnapshot(),
		"usagePath":    usageLedgerPath(),
		"cooldownMs":   effectiveCooldownTable(),
		"defaultAlias": defaultAutoRouterAlias,
	}
}

// freeModelsFor 该 provider 当前可用于自动路由的模型。
func freeModelsFor(name string, p *modelProvider) []catalogModel {
	if p == nil {
		return nil
	}
	out := p.freeModelIDs()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// effectiveCooldownTable 各类别的当前生效时长(配置优先)。
func effectiveCooldownTable() map[string]int64 {
	out := make(map[string]int64, len(defaultCooldownMs))
	for k := range defaultCooldownMs {
		out[k] = cooldownDurationMs(k)
	}
	if cfg := getZenConfig(); cfg != nil {
		for k, v := range cfg.CooldownMs {
			if v > 0 {
				out[k] = v
			}
		}
	}
	return out
}

// GET /admin/api/router
func handleAdminRouter(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: routerSnapshot()})
}

// routerSelectionRequest 保存/校验共用的请求体。
type routerSelectionRequest struct {
	Alias     string   `json:"alias"`
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
}

// normalizeRouterAlias 校验并规整别名。
func normalizeRouterAlias(alias string) (string, error) {
	a := strings.TrimSpace(alias)
	if a == "" {
		return "", fmt.Errorf("自动路由模型名不能为空")
	}
	if len(a) > 64 {
		return "", fmt.Errorf("自动路由模型名不能超过 64 个字符")
	}
	if !routerAliasRe.MatchString(a) {
		return "", fmt.Errorf("自动路由模型名只能包含字母、数字与 . _ / -，且不能以符号开头")
	}
	return a, nil
}

// cleanSelection 去空、去重、去重名(保持输入顺序)。
func cleanSelection(providers, models []string) ([]string, []string) {
	cl := func(in []string) []string {
		seen := map[string]bool{}
		out := make([]string, 0, len(in))
		for _, v := range in {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
		return out
	}
	return cl(providers), cl(models)
}

// validateRouterSelection 校验一次勾选, 返回问题列表(空 = 通过)。
//
// 校验的是"这份勾选提交后能不能真的工作", 而不是格式合法性:
// 选中了未配置 API Key 的供应商、选了目录里已经消失的模型, 都会在这里报出来。
func validateRouterSelection(sel routerSelectionRequest) []string {
	problems := []string{}
	for _, p := range sel.Providers {
		switch p {
		case upstreamZen:
			// 内置上游没有 provider 配置, 恒可参与
		case upstreamCline:
			if !clinePoolReady() {
				problems = append(problems, "Cline 账号池还没有可用账号，它的候选会被跳过")
			}
		default:
			pc, ok := providerConfigFor(p)
			if !ok {
				problems = append(problems, fmt.Sprintf("供应商 %s 已不存在，请重新勾选", p))
				continue
			}
			if pc.APIKey == "" {
				problems = append(problems, fmt.Sprintf("供应商 %s 没有 API Key，它的模型都会被跳过", p))
			}
		}
	}
	chosenProviders := map[string]bool{}
	for _, p := range sel.Providers {
		chosenProviders[p] = true
	}
	for _, m := range sel.Models {
		up, model, ok := strings.Cut(m, ":")
		if !ok || up == "" || model == "" {
			problems = append(problems, fmt.Sprintf("%q 不是合法的 上游:模型", m))
			continue
		}
		if !chosenProviders[up] {
			problems = append(problems, fmt.Sprintf("%s 属于未勾选的上游 %s", m, up))
			continue
		}
		switch up {
		case upstreamZen:
			if model == clinePoolPlaceholder {
				problems = append(problems, fmt.Sprintf("%q 不能作为 opencode 的模型名", model))
			} else if _, ok := resolveZenFreeModel(model); !ok {
				problems = append(problems, fmt.Sprintf("模型 %s 不是可用的 opencode 免费模型", m))
			}
		case upstreamCline:
			if model != clinePoolPlaceholder {
				problems = append(problems, fmt.Sprintf("cline 账号池只支持占位符 %q（%s）", clinePoolPlaceholder, m))
			}
		default:
			p := providerByName(up)
			if p == nil {
				problems = append(problems, fmt.Sprintf("供应商 %s 已不存在（%s）", up, m))
				continue
			}
			found := false
			for _, fm := range p.freeModelIDs() {
				if fm.ID == model {
					found = true
					break
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("模型 %s 不在 %s 当前可用列表里（可能已下架或目录未刷新）", model, up))
			}
		}
	}
	return problems
}

// POST /admin/api/router/save  { alias, providers[], models[] }
func handleAdminRouterSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	sel, err := decodeRouterSelection(r)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	alias, err := normalizeRouterAlias(sel.Alias)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	sel.Providers, sel.Models = cleanSelection(sel.Providers, sel.Models)

	previous := autoRouterAlias()
	mutateProvidersConfig(func(cfg *zenConfigData) {
		if cfg.Routes == nil {
			cfg.Routes = map[string][]string{}
		}
		// 改名时把旧别名下的候选链搬过去, 否则用户改个名字就把勾选丢了
		if previous != alias {
			if _, wasAuto := cfg.Routes[previous]; wasAuto {
				delete(cfg.Routes, previous)
			}
		}
		if len(sel.Models) > 0 {
			cfg.Routes[alias] = sel.Models
		} else {
			// 一个都不勾 = 回落到默认链(全部供应商的免费模型), 不写空列表
			delete(cfg.Routes, alias)
		}
		cfg.Router.Alias = alias
		cfg.Router.Providers = sel.Providers
	})

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"alias":    alias,
		"models":   len(sel.Models),
		"problems": validateRouterSelection(sel),
	}})
}

// POST /admin/api/router/validate  { alias, providers[], models[] }
func handleAdminRouterValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	sel, err := decodeRouterSelection(r)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	sel.Providers, sel.Models = cleanSelection(sel.Providers, sel.Models)
	if _, err := normalizeRouterAlias(sel.Alias); err != nil {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
			"problems": []string{err.Error()},
		}})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"problems": validateRouterSelection(sel),
		"models":   len(sel.Models),
	}})
}

// POST /admin/api/router/refresh 刷新全部供应商目录, 让模型列表是最新的。
func handleAdminRouterRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	names := providerNames()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	for _, name := range names {
		p := providerByName(name)
		if p == nil {
			continue
		}
		if err := p.refreshCatalog(ctx, true); err != nil {
			// 单个失败不阻塞其余: 页面会从各自 provider 的状态里看到原因
			continue
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"refreshed": len(names)}})
}

func decodeRouterSelection(r *http.Request) (routerSelectionRequest, error) {
	var sel routerSelectionRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return sel, err
	}
	defer r.Body.Close()
	if len(strings.TrimSpace(string(body))) == 0 {
		return sel, nil
	}
	if err := json.Unmarshal(body, &sel); err != nil {
		return sel, err
	}
	return sel, nil
}

// clearAllCandidateCooldowns 手动解除全部候选层冷却(面板按钮)。
func clearAllCandidateCooldowns() {
	candidateCoolMu.Lock()
	candidateCools = map[string]candidateCool{}
	candidateCoolMu.Unlock()
}

// clearAllCandidatePermanents 清掉永久剔除, 让之前被判死的候选重新参与。
func clearAllCandidatePermanents() {
	candidateCoolMu.Lock()
	candidatePerms = map[string]string{}
	candidateCoolMu.Unlock()
}

// permanentRejectionList 面板展示用: 永久剔除列表(排序)。
func permanentRejectionList() []map[string]string {
	candidateCoolMu.Lock()
	out := make([]map[string]string, 0, len(candidatePerms))
	for k, v := range candidatePerms {
		out = append(out, map[string]string{"key": k, "reason": v})
	}
	candidateCoolMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i]["key"] < out[j]["key"] })
	return out
}

// POST /admin/api/router/maintenance  { clearCooling?, clearPermanent? }
func handleAdminRouterMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ClearCooling   bool `json:"clearCooling"`
		ClearPermanent bool `json:"clearPermanent"`
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
	}
	if req.ClearCooling {
		clearAllCandidateCooldowns()
	}
	if req.ClearPermanent {
		clearAllCandidatePermanents()
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}
