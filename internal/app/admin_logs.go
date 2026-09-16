package app

import (
	"net/http"
	"strconv"
	"strings"
)

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	snap := poolSnapshot()
	active, cooldown, expired := 0, 0, 0
	for _, a := range snap.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    len(snap.Accounts),
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": "round_robin",
			"version":  buildVersion,
		},
	})
}

// GET /admin/api/accounts/export 导出全部账号 refreshToken（JSON 文件下载）
func handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	snap := poolSnapshot()
	items := make([]map[string]any, 0, len(snap.Accounts))
	for _, a := range snap.Accounts {
		items = append(items, map[string]any{
			"refreshToken": a.RefreshToken,
			"email":        a.Email,
		})
	}
	// 走统一的 apiResponse 信封, 前端 exportAccounts() 即可复用 api() 的错误处理/超时/中断,
	// 不再各自裸写 fetch。前端拿到 data 后自行构造 Blob 触发下载。
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: items})
}

// GET /admin/api/logs 最近请求日志（对话/调用历史）
// handleRequestLogs 请求日志查询(支持筛选与分页)。
//
// 查询参数:
//
//	q        关键词, 模糊匹配 id/model/resolvedModel/path/upstream/note/errMsg
//	upstream 精确匹配上游名(zen / cline / clinepass / provider/<name>)
//	status   "err"(>=400) | "ok"(<400) | 数字状态码
//	model    模型名子串(请求模型或实际模型)
//	page     页码, 从 1 起; pageSize 每页条数, 默认 50, 上限 500
//
// 返回 {logs, total, page, pageSize}: total 是筛选后的总数, 前端据此画分页。
func handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	upstream := strings.TrimSpace(r.URL.Query().Get("upstream"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	modelQ := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("model")))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 50
	}

	logs := LoadRequestLogs()
	// 倒序（最新在前）
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}

	filtered := make([]RequestLog, 0, len(logs))
	for _, l := range logs {
		if upstream != "" && l.Upstream != upstream && l.Route != upstream {
			continue
		}
		switch status {
		case "err":
			if l.Status < 400 {
				continue
			}
		case "ok":
			if l.Status >= 400 {
				continue
			}
		case "":
		default:
			if n, err := strconv.Atoi(status); err != nil || n != l.Status {
				continue
			}
		}
		if modelQ != "" &&
			!strings.Contains(strings.ToLower(l.Model), modelQ) &&
			!strings.Contains(strings.ToLower(l.ResolvedModel), modelQ) {
			continue
		}
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				l.ID, l.Model, l.ResolvedModel, l.Path, l.Upstream, l.Note, l.ErrMsg, l.ErrClass, l.Exit,
			}, "\x00"))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		filtered = append(filtered, l)
	}

	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageLogs := filtered[start:end]
	if pageLogs == nil {
		pageLogs = []RequestLog{}
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"logs":     pageLogs,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
}

// handleLogTrace 单条请求的路由决策轨迹(为什么选了这站/为什么跳过其它)。
// 轨迹只存路由元数据(TTL 30 分钟 / 最近 2000 条), 不含 prompt 与正文。
func handleLogTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	d := decisionTraceFor(id)
	if d == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "轨迹不存在或已过期(仅保留最近 30 分钟)"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"trace": d}})
}

// ============ 节点手动拉黑 (P2, 参照 easy_proxies 的手动黑名单) ============
