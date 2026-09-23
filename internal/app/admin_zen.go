package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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
		"enabled":             cfg.Enabled,
		"key":                 cfg.Key,
		"anonymous":           cfg.Anonymous,
		"baseURL":             cfg.BaseURL,
		"baseURLs":            zenBaseURLList(cfg),
		"proxies":             cfg.Proxies,
		"subs":                cfg.Subs,
		"exitMode":            cfg.ExitMode,
		"enabledRegions":      cfg.EnabledRegions,
		"regionSummary":       exitRegionSummary(),
		"dnsMode":             cfg.DNSMode,
		"dnsCustom":           cfg.DNSCustomDNS,
		"rescueDirect":        rescueDirectEnabled(),
		"subsRefreshMins":     cfg.SubsRefreshMins,
		"proxyStrategy":       cfg.ProxyStrategy,
		"streamIdleSecs":      cfg.StreamIdleSecs,
		"streamHeartbeatSecs": cfg.StreamHeartbeatSecs,
		"stickySessions":      cfg.StickySessions,
		"nodeExcludeKeywords": cfg.NodeExcludeKeywords,
		"maxConcurrency":      cfg.MaxConcurrency,
		"retries":             cfg.Retries,
		"failover":            cfg.Failover,
		"failoverCount":       cfg.FailoverCount,
		"failoverMinutes":     cfg.FailoverMinutes,
		"compaction":          cfg.Compaction,
		"runtime": func() map[string]any {
			// 只读快照: 这里**不能**调 zenFailedNow() —— 它会真实放行半开探测
			// (改熔断状态), 面板刷新一下就把一次性 CAS 的探测名额消费掉,
			// 卡住后续全部请求。展示统一走无副作用的 zenCircuitStatus。
			open, probing := zenCircuitStatus()
			return map[string]any{
				"failoverActive": open || probing,
				"proxyCooldowns": zenProxyCooldownStatus(),
				"subsStatus":     subStatusSnapshot(),
				"circuit":        map[string]any{"open": open, "probing": probing},
			}
		}(),
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

	// 全局配置的读取挪进下方 updateZenConfig 的同一临界区(P3-16):
	// 此前先 getZenConfig() 拿快照, 锁外改几十行再整体替换, 并发保存会丢更新。
	var patch struct {
		Enabled             *bool     `json:"enabled"`
		Key                 *string   `json:"key"`
		Anonymous           *bool     `json:"anonymous"`
		BaseURL             *string   `json:"baseURL"`
		BaseURLs            []string  `json:"baseURLs"`
		Proxies             []string  `json:"proxies"`
		Subs                []string  `json:"subs"`
		ExitMode            *string   `json:"exitMode"`
		StickySessions      *bool     `json:"stickySessions"`
		NodeExcludeKeywords *[]string `json:"nodeExcludeKeywords"`
		EnabledRegions      []string  `json:"enabledRegions"`
		DNSMode             *string   `json:"dnsMode"`
		DNSCustom           *string   `json:"dnsCustom"`
		RescueDirect        *bool     `json:"rescueDirect"`
		SubsRefreshMins     *int      `json:"subsRefreshMins"`
		ProxyStrategy       *string   `json:"proxyStrategy"`
		StreamIdleSecs      *int      `json:"streamIdleSecs"`
		StreamHeartbeatSecs *int      `json:"streamHeartbeatSecs"`
		MaxConcurrency      *int      `json:"maxConcurrency"`
		Retries             *int      `json:"retries"`
		Failover            *bool     `json:"failover"`
		FailoverCount       *int      `json:"failoverCount"`
		FailoverMinutes     *int      `json:"failoverMinutes"`
		Compaction          *struct {
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
	//
	// P3-16: 读-改-写整体收进 updateZenConfig 的同一把 zenConfigMu ——
	// 此前是 getZenConfig 拿快照 → 锁外改几十行 → setZenConfig 整体替换并落盘,
	// 读写窗口不在同一临界区, 并发保存时窗口期内另一方的修改被旧快照覆盖,
	// 重启后仍丢失。回调内不得再调 getZenConfig/setZenConfig(锁不可重入),
	// 校验失败以 error 返回, 不写回。
	var dnsChanged, subsChanged bool
	var subsToResolve []string
	if uerr := updateZenConfig(func(next *zenConfigData) error {
		// 修改前的快照: 供变更比较与 Compaction 合并基线。
		cur := next.clone()
		if patch.Enabled != nil {
			next.Enabled = *patch.Enabled
		}
		if patch.Anonymous != nil {
			next.Anonymous = *patch.Anonymous
		}
		if patch.Key != nil {
			// P3-17: 只认 TrimSpace 后的非空值 —— 旧判定 *patch.Key != "" 会
			// 放行 " " 这种纯空白 key 落盘, 之后 zenSelectKey TrimSpace 取到
			// 空串, 请求不带鉴权头 → 全量 401。
			k := strings.TrimSpace(*patch.Key)
			if k != "" {
				next.Key = k
			}
			// 落盘前断言(与 zenAllKeys 同一 TrimSpace 口径): 动过 key 字段后
			// key 集合必须非空, 否则所有 zen 请求都会裸奔(无鉴权头)。
			if len(zenAllKeys(next)) == 0 {
				return errors.New("zen key 不能为空: 至少配置一把可用 key")
			}
		}
		if patch.BaseURL != nil && *patch.BaseURL != "" {
			u := strings.TrimRight(strings.TrimSpace(*patch.BaseURL), "/")
			if err := validateOutboundURL(u); err != nil {
				return err
			}
			next.BaseURL = u
		}
		if patch.BaseURLs != nil {
			// 端点列表整体替换;空数组 = 恢复默认(官方 + 全部镜像)。
			// 走统一的出站地址校验: 这些端点由服务端主动请求, 只查 "http://" 前缀
			// 挡不住云元数据地址(http://169.254.169.254/...)。
			cleaned, err := filterOutboundURLs(patch.BaseURLs, "端点")
			if err != nil {
				return err
			}
			next.BaseURLs = cleaned
			// 主端点同步为列表第一个,保持旧字段语义
			if len(cleaned) > 0 {
				next.BaseURL = cleaned[0]
			}
		}
		if patch.Proxies != nil {
			if err := validateProxyList(patch.Proxies); err != nil {
				return err
			}
			next.Proxies = patch.Proxies
		}
		if patch.Subs != nil {
			// 同理: 订阅是服务端去抓取的地址。
			cleaned, err := filterOutboundURLs(patch.Subs, "订阅")
			if err != nil {
				return err
			}
			next.Subs = cleaned
		}
		if patch.ExitMode != nil && *patch.ExitMode != "" {
			if *patch.ExitMode != exitModeDirect && *patch.ExitMode != exitModeProxy {
				return errors.New("出口模式无效（需 direct 或 proxy）")
			}
			next.ExitMode = *patch.ExitMode
		}
		if patch.StickySessions != nil {
			next.StickySessions = *patch.StickySessions
		}
		if patch.NodeExcludeKeywords != nil {
			cleaned := make([]string, 0, len(*patch.NodeExcludeKeywords))
			for _, k := range *patch.NodeExcludeKeywords {
				if k = strings.TrimSpace(k); k != "" {
					cleaned = append(cleaned, k)
				}
			}
			next.NodeExcludeKeywords = cleaned
		}
		if patch.EnabledRegions != nil {
			// 只允许 7 个已知地区; 空数组 = 清除限制(全部地区可用)。
			// 未知 ID 直接 400, 避免拼错导致"勾了但一个出口都不剩"这种静默故障。
			for _, id := range patch.EnabledRegions {
				if !validExitRegion(strings.ToLower(strings.TrimSpace(id))) {
					return errors.New("地区无效: " + id + "（可选: us/jp/tw/hk/sg/eu/other）")
				}
			}
			next.EnabledRegions = normalizeExitRegions(patch.EnabledRegions)
		}
		if patch.SubsRefreshMins != nil {
			mins := *patch.SubsRefreshMins
			if mins < subRefreshMinMins || mins > subRefreshMaxMins {
				return fmt.Errorf("订阅刷新间隔需在 %d~%d 分钟之间", subRefreshMinMins, subRefreshMaxMins)
			}
			next.SubsRefreshMins = mins
		}
		if patch.ProxyStrategy != nil && *patch.ProxyStrategy != "" {
			next.ProxyStrategy = *patch.ProxyStrategy
		}
		if patch.StreamIdleSecs != nil {
			// 0 = 恢复默认(90 秒); 上限 1800 防手滑配成"永久挂起"。
			secs := *patch.StreamIdleSecs
			if secs < 0 || secs > 1800 {
				return errors.New("流空闲上限需在 0~1800 秒之间（0=默认90秒）")
			}
			next.StreamIdleSecs = secs
		}
		if patch.StreamHeartbeatSecs != nil {
			// 与 StreamIdleSecs 完全同构(P3-20): 此前 streamHeartbeatSecs 没有任
			// 何 admin 读写口, 只能手改 JSON。0 = 关闭心跳注入; 上限 300 秒 ——
			// 已远超所有客户端 30~120s 的读超时, 再大没有意义。
			secs := *patch.StreamHeartbeatSecs
			if secs < 0 || secs > 300 {
				return errors.New("流式心跳间隔需在 0~300 秒之间（0=关闭，15=默认）")
			}
			next.StreamHeartbeatSecs = secs
		}
		if patch.MaxConcurrency != nil && *patch.MaxConcurrency > 0 {
			// P3-18: 与同文件其它数值字段一样必须有界 —— 无上限时手滑填个天文
			// 数字等于关掉并发准入控制。
			if *patch.MaxConcurrency > 64 {
				return errors.New("最大并发需在 0~64 之间")
			}
			next.MaxConcurrency = *patch.MaxConcurrency
		}
		if patch.Retries != nil && *patch.Retries >= 0 {
			if *patch.Retries > 10 {
				return errors.New("重试次数需在 0~10 之间")
			}
			next.Retries = *patch.Retries
		}
		if patch.Failover != nil {
			next.Failover = *patch.Failover
		}
		if patch.FailoverCount != nil && *patch.FailoverCount > 0 {
			if *patch.FailoverCount > 100 {
				return errors.New("故障转移触发次数需在 0~100 之间")
			}
			next.FailoverCount = *patch.FailoverCount
		}
		if patch.FailoverMinutes != nil && *patch.FailoverMinutes > 0 {
			// P3-18: 把"秒"当"分钟"填 86400 → 熔断开白数月, 所有 zen 免费请求
			// 静默转投 cline 付费池烧钱, 且 markZenSuccess 永不触发无法自愈。
			if *patch.FailoverMinutes > 60 {
				return errors.New("故障转移窗口需在 0~60 分钟之间")
			}
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
				return fmt.Errorf("DNS 模式无效（需 %s / %s / %s / %s）",
					dnsModeSystem, dnsModeDoHAli, dnsModeDoHCF, dnsModeCustom)
			}
			next.DNSMode = mode
		}
		if patch.DNSCustom != nil {
			custom := strings.TrimSpace(*patch.DNSCustom)
			if custom != "" {
				if _, _, _, err := parseDoHURL(custom); err != nil {
					return fmt.Errorf("自定义 DoH 地址无效: %v", err)
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
		dnsChanged = (patch.DNSMode != nil && next.DNSMode != cur.DNSMode) ||
			(patch.DNSCustom != nil && next.DNSCustomDNS != cur.DNSCustomDNS)
		subsChanged = (patch.Subs != nil && !strSliceEqual(patch.Subs, cur.Subs)) || exitChanged
		subsToResolve = next.Subs
		return nil
	}); uerr != nil {
		// 校验失败: 回调返回错误即不写回, 配置保持原样。
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: uerr.Error()})
		return
	}
	if dnsChanged {
		// DNS 段变了要重建单例, 否则新解析器不会生效
		go syncNodeBox()
	}
	if subsChanged {
		go resolveSubscriptions(subsToResolve)
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

// GET /admin/api/zen/models — 返回**全部**已同步的 zen 模型, 并逐个标注:
//   - free:    自动免费(seed 白名单 / -free 后缀), 面板上锁定勾选
//   - enabled: 当前对网关可用(自动免费或用户手动启用), 未启用的可在面板勾选启用
//   - manually: 用户手动启用的非自动免费模型(可随时取消勾选)
//
// 之前只返回免费模型, opencode 不定期放进的免费测试模型(如 union-alpha,
// 不带 -free 后缀)在目录里拉得到、却永远不显示, 用户无从启用。
func handleZenModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initZenModels()
	zenModelsMu.RLock()
	models := make([]map[string]any, 0, len(zenModels))
	for _, m := range zenModels {
		// 展示层必须用"自动免费"口径: isZenFreeModel 会把手动启用也算作免费,
		// 那样面板会把一个手动启用的测试模型渲染成自动免费、勾选框锁死,
		// 用户再也无法取消(2026-09-17 审查 C1)。
		autoFree := isAutoFreeZenModel(m)
		enabled := (autoFree || zenModelEnabled(m.ID)) && !zenModelUnavailable(m.ID)
		models = append(models, map[string]any{
			"id":       m.ID,
			"aliases":  m.Aliases,
			"context":  m.Context,
			"output":   m.Output,
			"source":   m.Source,
			"free":     autoFree,
			"enabled":  enabled,
			"manually": !autoFree && zenModelEnabled(m.ID), // 用户手动启用的非免费模型
		})
	}
	zenModelsMu.RUnlock()
	// 按 ID 排序, 保证面板上新增的测试模型位置稳定、不乱跳
	sort.Slice(models, func(i, j int) bool {
		return models[i]["id"].(string) < models[j]["id"].(string)
	})
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": models, "count": len(models)}})
}

// POST /admin/api/zen/models/toggle — 启用/禁用一个非自动免费的 zen 模型。
//
// body: {"model": "union-alpha", "enabled": true}
// 自动免费的模型(seed / -free 后缀)不需要也不能从这里关闭 —— 它们本来就免费,
// 开关对它们无效; 能操作的是目录里拉到、但没过免费判定的测试模型。
// 写入 cfg.EnabledModels 并立即刷缓存(isZenFreeModel 热路径读缓存集合)。
func handleZenModelsToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		Model   string `json:"model"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	id := strings.TrimSpace(req.Model)
	if id == "" || req.Enabled == nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "需要 model 与 enabled 字段"})
		return
	}
	initZenModels()
	// 必须是目录里真实存在的模型: 防止拼错或随手填一个不存在的 ID 进启用表。
	// 例外(P3-21): 已在启用表里的 ID 即使已从目录消失, 也必须允许**取消**启用 ——
	// 否则下架模型无法从面板摘除, 它的请求会因 resolveZenModel 失败裸落到
	// cline 付费上游, 与"这是 zen 免费模型"的用户预期相反。
	zenModelsMu.RLock()
	m, exists := zenModels[id]
	zenModelsMu.RUnlock()
	// 校验(只读)先做完; 下面的读-改-写放进 updateZenConfig 同一临界区 ——
	// 此前是 getZenConfig 快照 → 锁外构造 → setZenConfig 整体替换, 并发的
	// config update / 另一次 toggle 会互相覆盖(P3 同型丢更新竞态)。
	cur := getZenConfig()
	listed := false
	for _, e := range cur.EnabledModels {
		if strings.TrimSpace(e) == id {
			listed = true
			break
		}
	}
	if !exists && !listed {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: fmt.Sprintf("模型 %q 不在 zen 目录中(等待目录同步或名称有误)", id)})
		return
	}
	// 启用仍要求模型在目录中: 对"仅存于启用表"的 ID 只放行 disable。
	if !exists && *req.Enabled {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: fmt.Sprintf("模型 %q 不在 zen 目录中, 无法启用(仅可取消已启用的)", id)})
		return
	}
	// 自动免费的模型不能从这里关: 它们本来就免费, 关掉只会让用户误以为"关了还能用"
	// (仅当模型在目录里时判定 —— 已下架模型的 m 为 nil)。
	if exists && isZenFreeModel(m) && m.Source != "synced" {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: fmt.Sprintf("模型 %q 本就是免费模型, 无需手动启用", id)})
		return
	}
	if uerr := updateZenConfig(func(next *zenConfigData) error {
		set := make(map[string]bool, len(next.EnabledModels))
		for _, e := range next.EnabledModels {
			if e = strings.TrimSpace(e); e != "" {
				set[e] = true
			}
		}
		if *req.Enabled {
			set[id] = true
		} else {
			delete(set, id)
		}
		list := make([]string, 0, len(set))
		for e := range set {
			list = append(list, e)
		}
		sort.Strings(list)
		next.EnabledModels = list
		return nil
	}); uerr != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: uerr.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("模型 %q 已%s", id, map[bool]string{true: "启用", false: "禁用"}[*req.Enabled]),
		Data:    map[string]any{"enabled": *req.Enabled},
	})
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
