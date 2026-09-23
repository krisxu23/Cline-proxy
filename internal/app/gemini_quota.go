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
	quotaLineRe = regexp.MustCompile(`(?i)Quota exceeded for metric:\s*([^,]+),\s*limit:\s*(\d+)`)
	// 指标名两种拼写都要认: Google 的 quotaMetric 是 snake_case 的 dotted 指标
	// (generativelanguage.googleapis.com/generate_content_free_tier_requests),
	// 而 quotaId 与部分回包是 CamelCase(...-FreeTier / ...RequestsPerDay...)。
	freeTierMetricRe = regexp.MustCompile(`(?i)free[_]?tier`)
	requestMetricRe  = regexp.MustCompile(`(?i)_requests$|requests?[_-]?per[_-]?day`)
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
		limit, err := strconv.Atoi(m[2])
		if err != nil {
			// 解析失败就跳过这条指标: 归零会被下游读成"该模型无免费层"(永久剔除)。
			continue
		}
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
				// 两侧都去空白, 否则 message 与 quotaMetric 的键名对不上,
				// aligned 落假, 每日限额会被静默丢弃。
				metric := strings.TrimSpace(v.QuotaMetric)
				violationsByMetric[metric] = append(violationsByMetric[metric], v.QuotaID)
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
		// 对不齐时 quotaID 留空会让窗口判成"", 日配额耗尽被降级成 10 分钟
		// 冷却(当天每 10 分钟白撞 429)。同一指标的 violation 窗口通常一致:
		// 全部一致就整体借用 —— 窗口只看 PerDay/PerMinute 子串, 不需逐条对应。
		metricWindow, uniform := "", len(ids) > 0
		for _, id := range ids {
			w := quotaWindow(id)
			if w == "" || (metricWindow != "" && w != metricWindow) {
				uniform = false
				break
			}
			metricWindow = w
		}
		if !uniform {
			metricWindow = ""
		}
		for i, e := range entries {
			if aligned {
				e.quotaID = ids[i]
			}
			e.window = quotaWindow(e.quotaID)
			if e.window == "" {
				e.window = metricWindow
			}
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

	// 真正触发 429 的是 details.violations —— message 行只是限额罗列, 常同时
	// 含 day 与 minute 两行: 逐行挑第一个 limit>0 会把 minute 级限流误判成
	// 日配额(冷却到太平洋午夜, 过冷), 或反过来(日耗尽只冷 10 分钟, 白撞)。
	violatedDay, violatedMinute := false, false
	for metric, ids := range violationsByMetric {
		if !freeTierMetricRe.MatchString(metric) {
			continue
		}
		positive := false
		for _, e := range byMetric[metric] {
			if e.limit > 0 {
				positive = true
				break
			}
		}
		if !positive {
			continue // limit 0 = 无免费层, 窗口语义归 NoFreeTier
		}
		for _, id := range ids {
			switch quotaWindow(id) {
			case "day":
				violatedDay = true
			case "minute":
				violatedMinute = true
			}
		}
	}
	exhausted := ""
	switch {
	case violatedDay:
		exhausted = "day"
	case violatedMinute:
		exhausted = "minute"
	default:
		// 没有可判窗的 violation: 退回 message 行 —— 有 day 行按 day,
		// 否则任何 free 限额行按 minute(窗口未知时的保守短冷却)。
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
