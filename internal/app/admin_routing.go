package app

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// 候选链 / 账本 / 发现 的面板接口。
//
// 三块内容合成一个 GET: 面板刷新一次就能把"路由顺序、冷却中的候选、
// 今日用量、发现状态"全部更新, 不必发四个请求。

// GET /admin/api/routing
func handleAdminRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	aliases := routeAliasNames()
	routes := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		routes = append(routes, describeRouteChain(a))
	}
	cfg := getZenConfig()
	cooldownCfg := map[string]int64{}
	for k := range defaultCooldownMs {
		cooldownCfg[k] = cooldownDurationMs(k)
	}
	if cfg != nil {
		for k, v := range cfg.CooldownMs {
			if v > 0 {
				cooldownCfg[k] = v
			}
		}
	}
	discoveredMu.Lock()
	discoveredN := len(discoveredModels)
	discoveredMu.Unlock()

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"routes":    routes,
		"cooling":   candidateCoolingSnapshot(),
		"usage":     usageSnapshot(),
		"cooldown":  cooldownCfg,
		"permanent": permanentRejectionList(),
		"discovery": map[string]any{
			"config":     discoveryConfig(),
			"discovered": discoveredN,
			"path":       discoveredFilePath(),
		},
		"usagePath": usageLedgerPath(),
	}})
}

// POST /admin/api/routing/update
// body: { routes?, cooldownMs?, usage?, discovery?, clearCooling?, clearPermanent? }
//
// 全部字段可选: 只改传进来的那部分, 没传的保持原值 —— 避免面板某次
// 局部保存把其他设置清空。
func handleAdminRoutingUpdate(w http.ResponseWriter, r *http.Request) {
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
		Routes         map[string][]string `json:"routes"`
		CooldownMs     map[string]int64    `json:"cooldownMs"`
		Usage          *zenUsageConfig     `json:"usage"`
		Discovery      *zenDiscoveryConfig `json:"discovery"`
		ClearCooling   bool                `json:"clearCooling"`
		ClearPermanent bool                `json:"clearPermanent"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}

	mutateProvidersConfig(func(cfg *zenConfigData) {
		if req.Routes != nil {
			cfg.Routes = sanitizeRoutes(req.Routes)
		}
		if req.CooldownMs != nil {
			clean := map[string]int64{}
			for k, v := range req.CooldownMs {
				if v > 0 {
					clean[k] = v
				}
			}
			cfg.CooldownMs = clean
		}
		if req.Usage != nil {
			u := *req.Usage
			if u.Timezone == "" {
				u.Timezone = defaultUsageTimezone
			}
			if u.RetentionDays <= 0 {
				u.RetentionDays = defaultUsageRetentionDays
			}
			cfg.Usage = u
		}
		if req.Discovery != nil {
			d := *req.Discovery
			if d.Provider == "" {
				d.Provider = "openrouter"
			}
			cfg.Discovery = d
		}
	})

	if req.ClearCooling {
		clearAllCandidateCooldowns()
	}
	if req.ClearPermanent {
		clearAllCandidatePermanents()
		saveDiscovered()
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// sanitizeRoutes 去掉空别名与空条目, 并裁掉条目两端空白。
func sanitizeRoutes(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for alias, list := range in {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		clean := make([]string, 0, len(list))
		for _, e := range list {
			if e = strings.TrimSpace(e); e != "" {
				clean = append(clean, e)
			}
		}
		out[alias] = clean
	}
	return out
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
	snap := candidatePermSnapshot()
	out := make([]map[string]string, 0, len(snap))
	for k, v := range snap {
		out = append(out, map[string]string{"key": k, "reason": v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["key"] < out[j]["key"] })
	return out
}
