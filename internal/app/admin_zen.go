package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ============ Zen 免费模型管理 API ============

// GET /admin/api/zen/config
func handleZenConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	cfg := getZenConfig()
	data := map[string]any{
		"enabled":         cfg.Enabled,
		"key":             cfg.Key,
		"baseURL":         cfg.BaseURL,
		"baseURLs":        zenBaseURLList(cfg),
		"proxies":         cfg.Proxies,
		"subs":            cfg.Subs,
		"exitMode":        cfg.ExitMode,
		"dnsMode":         cfg.DNSMode,
		"dnsCustom":       cfg.DNSCustomDNS,
		"rescueDirect":    rescueDirectEnabled(),
		"subsRefreshMins": cfg.SubsRefreshMins,
		"proxyStrategy":   cfg.ProxyStrategy,
		"maxConcurrency":  cfg.MaxConcurrency,
		"retries":         cfg.Retries,
		"failover":        cfg.Failover,
		"failoverCount":   cfg.FailoverCount,
		"failoverMinutes": cfg.FailoverMinutes,
		"compaction":      cfg.Compaction,
		"runtime": map[string]any{
			"failoverActive": zenFailedNow(),
			"proxyCooldowns": zenProxyCooldownStatus(),
			"subsStatus":     subStatusSnapshot(),
			"circuit": func() map[string]any {
				open, probing := zenCircuitStatus()
				return map[string]any{"open": open, "probing": probing}
			}(),
		},
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

// POST /admin/api/zen/config/update
func handleZenConfigUpdate(w http.ResponseWriter, r *http.Request) {
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

	cur := getZenConfig()
	if cur == nil {
		cur = defaultZenConfig()
	}
	var patch struct {
		Enabled         *bool    `json:"enabled"`
		Key             *string  `json:"key"`
		BaseURL         *string  `json:"baseURL"`
		BaseURLs        []string `json:"baseURLs"`
		Proxies         []string `json:"proxies"`
		Subs            []string `json:"subs"`
		ExitMode        *string  `json:"exitMode"`
		DNSMode         *string  `json:"dnsMode"`
		DNSCustom       *string  `json:"dnsCustom"`
		RescueDirect    *bool    `json:"rescueDirect"`
		SubsRefreshMins *int     `json:"subsRefreshMins"`
		ProxyStrategy   *string  `json:"proxyStrategy"`
		MaxConcurrency  *int     `json:"maxConcurrency"`
		Retries         *int     `json:"retries"`
		Failover        *bool    `json:"failover"`
		FailoverCount   *int     `json:"failoverCount"`
		FailoverMinutes *int     `json:"failoverMinutes"`
		Compaction      *struct {
			Auto         *bool   `json:"auto"`
			Buffer       *int    `json:"buffer"`
			KeepTokens   *int    `json:"keepTokens"`
			SummaryModel *string `json:"summaryModel"`
			MaxSummary   *int    `json:"maxSummary"`
		} `json:"compaction"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	// 从完整的当前配置出发, 只覆盖请求里显式出现的字段。
	//
	// 这里曾经是 `next := &zenConfigData{...}` 手工列举 16 个字段。结构体现有
	// 23 个字段, 于是 Routes(候选链) / Router(自动路由) / Usage(每日配额账本) /
	// CooldownMs / DNSMode / DNSCustomDNS / RescueDirect 每次保存都被零值覆盖
	// 并落盘 —— 在 zen 设置页改任意无关项(例如重试次数), 用户在自动路由页配好的
	// 候选链就直接消失, RescueDirect 归 nil 还会把"节点全挂时直连兜底"悄悄
	// 重新打开, 绕开统一出口。改用 clone 之后新增字段不可能再被漏掉。
	next := cur.clone()
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if patch.Key != nil && *patch.Key != "" {
		next.Key = *patch.Key
	}
	if patch.BaseURL != nil && *patch.BaseURL != "" {
		u := strings.TrimRight(strings.TrimSpace(*patch.BaseURL), "/")
		if err := validateOutboundURL(u); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		next.BaseURL = u
	}
	if patch.BaseURLs != nil {
		// 端点列表整体替换;空数组 = 恢复默认(官方 + 全部镜像)。
		// 走统一的出站地址校验: 这些端点由服务端主动请求, 只查 "http://" 前缀
		// 挡不住云元数据地址(http://169.254.169.254/...)。
		cleaned, err := filterOutboundURLs(patch.BaseURLs, "端点")
		if err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		next.BaseURLs = cleaned
		// 主端点同步为列表第一个,保持旧字段语义
		if len(cleaned) > 0 {
			next.BaseURL = cleaned[0]
		}
	}
	if patch.Proxies != nil {
		if err := validateProxyList(patch.Proxies); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		next.Proxies = patch.Proxies
	}
	if patch.Subs != nil {
		// 同理: 订阅是服务端去抓取的地址。
		cleaned, err := filterOutboundURLs(patch.Subs, "订阅")
		if err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		next.Subs = cleaned
	}
	if patch.ExitMode != nil && *patch.ExitMode != "" {
		if *patch.ExitMode != exitModeDirect && *patch.ExitMode != exitModeProxy {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "出口模式无效（需 direct 或 proxy）"})
			return
		}
		next.ExitMode = *patch.ExitMode
	}
	if patch.SubsRefreshMins != nil {
		mins := *patch.SubsRefreshMins
		if mins < subRefreshMinMins || mins > subRefreshMaxMins {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: fmt.Sprintf("订阅刷新间隔需在 %d~%d 分钟之间", subRefreshMinMins, subRefreshMaxMins)})
			return
		}
		next.SubsRefreshMins = mins
	}
	if patch.ProxyStrategy != nil && *patch.ProxyStrategy != "" {
		next.ProxyStrategy = *patch.ProxyStrategy
	}
	if patch.MaxConcurrency != nil && *patch.MaxConcurrency > 0 {
		next.MaxConcurrency = *patch.MaxConcurrency
	}
	if patch.Retries != nil && *patch.Retries >= 0 {
		next.Retries = *patch.Retries
	}
	if patch.Failover != nil {
		next.Failover = *patch.Failover
	}
	if patch.FailoverCount != nil && *patch.FailoverCount > 0 {
		next.FailoverCount = *patch.FailoverCount
	}
	if patch.FailoverMinutes != nil && *patch.FailoverMinutes > 0 {
		next.FailoverMinutes = *patch.FailoverMinutes
	}
	if patch.Compaction != nil {
		base := cur.Compaction
		if patch.Compaction.Buffer != nil {
			base.Buffer = *patch.Compaction.Buffer
		}
		if patch.Compaction.KeepTokens != nil {
			base.KeepTokens = *patch.Compaction.KeepTokens
		}
		if patch.Compaction.SummaryModel != nil {
			base.SummaryModel = *patch.Compaction.SummaryModel
		}
		if patch.Compaction.MaxSummary != nil {
			base.MaxSummary = *patch.Compaction.MaxSummary
		}
		if patch.Compaction.Auto != nil {
			base.Auto = *patch.Compaction.Auto
		}
		next.Compaction = base
	}
	if patch.DNSMode != nil && *patch.DNSMode != "" {
		mode := *patch.DNSMode
		if normalizeDNSMode(mode) != mode {
			writeAPI(w, http.StatusBadRequest, apiResponse{
				Error: fmt.Sprintf("DNS 模式无效（需 %s / %s / %s / %s）",
					dnsModeSystem, dnsModeDoHAli, dnsModeDoHCF, dnsModeCustom)})
			return
		}
		next.DNSMode = mode
	}
	if patch.DNSCustom != nil {
		custom := strings.TrimSpace(*patch.DNSCustom)
		if custom != "" {
			if _, _, _, err := parseDoHURL(custom); err != nil {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: fmt.Sprintf("自定义 DoH 地址无效: %v", err)})
				return
			}
		}
		next.DNSCustomDNS = custom
	}
	if patch.RescueDirect != nil {
		v := *patch.RescueDirect
		next.RescueDirect = &v
	}
	// 订阅增删, 或出口模式切换(抓取路径随之改变)都重新抓取; 空列表会清空订阅节点
	exitChanged := patch.ExitMode != nil && *patch.ExitMode != cur.ExitMode
	dnsChanged := (patch.DNSMode != nil && next.DNSMode != cur.DNSMode) ||
		(patch.DNSCustom != nil && next.DNSCustomDNS != cur.DNSCustomDNS)
	subsChanged := (patch.Subs != nil && !strSliceEqual(patch.Subs, cur.Subs)) || exitChanged
	setZenConfig(next)
	if dnsChanged {
		// DNS 段变了要重建单例, 否则新解析器不会生效
		go syncNodeBox()
	}
	if subsChanged {
		go resolveSubscriptions(next.Subs)
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: getZenConfig()})
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// GET /admin/api/opencode/nodes — 出口池节点列表(手动+订阅)
func handleZenNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: nodeViews()})
}

// POST /admin/api/opencode/nodes/check — 触发全部出口的真实连通检测(异步)
func handleZenNodesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	go checkAllNodeHealth()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "连通检测已启动"})
}

// GET /admin/api/zen/models — 只返回免费模型
func handleZenModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initZenModels()
	zenModelsMu.RLock()
	models := make([]map[string]any, 0, len(zenModels))
	for _, m := range zenModels {
		if !isZenFreeModel(m) || zenModelUnavailable(m.ID) {
			continue
		}
		models = append(models, map[string]any{
			"id":      m.ID,
			"aliases": m.Aliases,
			"context": m.Context,
			"output":  m.Output,
			"source":  m.Source,
		})
	}
	zenModelsMu.RUnlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": models, "count": len(models)}})
}

// POST /admin/api/zen/models/refresh
func handleZenModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	added, err := syncZenModels()
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: "sync failed: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: fmt.Sprintf("synced, %d new models", added)})
}

// GET /admin/api/zen/stats
func handleZenStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: zenStatsSnapshot()})
}
