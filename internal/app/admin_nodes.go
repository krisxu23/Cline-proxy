package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// handleNodeBlacklist POST /admin/api/nodes/blacklist
// body: {"key": "<nodeLocalKey>", "hours": 24}  hours<=0 或缺省 = 长期
func handleNodeBlacklist(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Key   string  `json:"key"`
		Hours float64 `json:"hours"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body 必须是 {\"key\": \"...\", \"hours\": 24}"})
		return
	}
	blacklistNode(strings.TrimSpace(body.Key), time.Duration(body.Hours*float64(time.Hour)))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"blacklisted": body.Key, "list": blacklistList()}})
}

// handleNodeBlacklistClear POST /admin/api/nodes/blacklist/clear
// body: {"key": "..."} 或 {"all": true}
func handleNodeBlacklistClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Key string `json:"key"`
		All bool   `json:"all"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body)
	manualBlackMu.Lock()
	n := len(manualBlack)
	key := strings.TrimSpace(body.Key)
	if body.All {
		manualBlack = map[string]time.Time{}
	} else if key != "" {
		delete(manualBlack, key)
	}
	manualBlackMu.Unlock()
	// 破坏性操作留痕(审计 P3): 全清带数量, 单删带标识。
	if body.All {
		log.Printf("Node blacklist cleared (%d)", n)
	} else if key != "" {
		log.Printf("Node blacklist deleted: %q", key)
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"list": blacklistList()}})
}

// handleNodeBlacklistList GET /admin/api/nodes/blacklist
func handleNodeBlacklistList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"list": blacklistList()}})
}

// handleHealth 网关健康总览 (P2-20, 参照 OmniRoute 的 ProviderHealthAutopilotCard):
//
// 不是单一布尔, 而是 {state, score, signals, issues[]} —— 每条 issue 携带
// severity / recommendation / evidence, 让面板能回答"哪里不健康、为什么、
// 建议做什么"。全部信号来自现有冷却/健康/配置状态, 零额外请求。
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var issues []map[string]any
	score := 100
	addIssue := func(severity, component, problem, recommendation, evidence string) {
		issues = append(issues, map[string]any{
			"severity":       severity, // critical | warn | info
			"component":      component,
			"problem":        problem,
			"recommendation": recommendation,
			"evidence":       evidence,
		})
	}

	// 1) zen 主通道
	cfg := getZenConfig()
	zenReady := false
	if cfg.Enabled {
		if cfg.Key == "" {
			addIssue("critical", "zen", "opencode zen 未配置 key", "在「供应商管理」填写 zen API key, 或使用 public 匿名档", "zen.enabled=true 但 key 为空")
			score -= 30
		} else {
			zenReady = true
		}
	} else {
		addIssue("warn", "zen", "opencode zen 通道已停用", "如需使用 zen 免费模型, 在供应商管理里启用", "zen.enabled=false")
		score -= 10
	}

	// 2) cline 账号池
	if !clinePoolReady() {
		addIssue("warn", "cline", "cline 账号池当前无可用账号", "检查账号冷却状态或补充账号", "clinePoolReady()=false")
		score -= 15
	}

	// 3) ClinePass
	if !clinePassReady() {
		addIssue("info", "clinepass", "ClinePass 订阅池当前无可用 key", "如需 cline-pass/ 模型参与路由, 补充订阅 key", "clinePassReady()=false")
		score -= 5
	}

	// 4) 通用 Provider: 有目录但没 key 的要提醒
	for _, name := range providerNames() {
		pc, ok := providerConfigFor(name)
		if !ok {
			continue
		}
		if len(enabledAPIKeys(pc, name)) == 0 {
			if p := providerByName(name); p != nil && len(freeModelsFor(name, p)) > 0 {
				addIssue("warn", "provider/"+name, "供应商有可用模型但未配置 API Key", "在供应商管理里填写 "+name+" 的 key, 否则其模型不参与路由", "keys=0, models>0")
				score -= 10
			}
		}
	}

	// 5) 冷却/剔除/模型摘除计数
	candidateCoolMu.Lock()
	cooling, probing := 0, 0
	for _, c := range candidateCools {
		if time.Now().UnixMilli() < c.until {
			cooling++
		}
		if c.probing {
			probing++
		}
	}
	permanents := len(candidatePerms)
	candidateCoolMu.Unlock()
	if cooling > 0 {
		addIssue("info", "router", fmt.Sprintf("%d 个候选处于冷却", cooling), "冷却到期后半开探测会自动恢复; 持续冷却的候选请在路由页检查", fmt.Sprintf("cooling=%d", cooling))
	}
	if permanentlyRemoved := permanents; permanentlyRemoved > 0 {
		addIssue("info", "router", fmt.Sprintf("%d 个候选被永久剔除(无免费层/已下架)", permanentlyRemoved), "确认是否为预期行为; 误剔除可在路由页清空", fmt.Sprintf("permanent=%d", permanentlyRemoved))
	}

	// state 判定
	state := "ok"
	if score < 60 {
		state = "critical"
	} else if score < 90 {
		state = "warn"
	}
	if issues == nil {
		issues = []map[string]any{}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"state": state,
		"score": score,
		"signals": map[string]any{
			"zenReady":         zenReady,
			"clinePoolReady":   clinePoolReady(),
			"clinePassReady":   clinePassReady(),
			"cooling":          cooling,
			"probing":          probing,
			"permanentRemoved": permanents,
			"decisionTraces":   decisionTraceCount(),
		},
		"issues": issues,
	}})
}

// ============ 配置整体导出 / 导入 (P2-25) ============
