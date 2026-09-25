package app

import (
	"context"
	"fmt"
	"free-router/internal/cline"
	"free-router/internal/kit"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ModelStatus string

const (
	ModelActive  ModelStatus = "active"
	ModelRemoved ModelStatus = "removed"
	ModelUnknown ModelStatus = "unknown"
)

type ModelInfo struct {
	ID             string      `json:"id"`
	Name           string      `json:"name,omitempty"`
	Source         string      `json:"source"`
	Provider       string      `json:"provider"`
	Cost           string      `json:"cost"`
	Status         ModelStatus `json:"status"`
	RequiresStream bool        `json:"requiresStream,omitempty"`
	SyncedAt       time.Time   `json:"syncedAt,omitempty"`
}

var (
	modelsMu       sync.Mutex
	modelsCache    map[string]*ModelInfo
	modelsSyncing  bool
	modelsLastSync time.Time
)

const (
	modelsRefreshInterval = 60 * time.Second
	modelsSyncTimeout     = 25 * time.Second
)

const recommendedModelsURL = cline.ClineAPIBase + "/ai/cline/recommended-models"

func seedModelCandidates() []*ModelInfo {
	return []*ModelInfo{
		{ID: "deepseek/deepseek-v4-flash", Source: "free", Provider: "deepseek", Cost: "free", RequiresStream: true},
		{ID: "poolside/laguna-s-2.1:free", Source: "free", Provider: "poolside", Cost: "free"},
		{ID: "stepfun/step-3.7-flash", Source: "free", Provider: "stepfun", Cost: "free", RequiresStream: true},
	}
}

func initModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsCache != nil {
		return
	}
	modelsCache = make(map[string]*ModelInfo)
	for _, m := range seedModelCandidates() {
		modelsCache[m.ID] = m
	}
}

// modelsCacheSnapshot 持锁返回 modelsCache 的一份独立深拷贝。
// 任何需要从 modelsCache 读取多字段 / 多次读取的调用方都应走这个 accessor,
// 而不是在锁外裸读全局 map(会与 syncRecommendedModels 的并发写形成数据竞争,
// 见 §2.1 / §5.9)。返回的拷贝与全局状态隔离, 调用方可在锁外安全使用。
func modelsCacheSnapshot() map[string]*ModelInfo {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make(map[string]*ModelInfo, len(modelsCache))
	for k, v := range modelsCache {
		cp := *v
		out[k] = &cp
	}
	return out
}

func getFreeModels() []*ModelInfo {
	initModelsCache()
	// 走一致的快照 accessor, 不在锁外裸读 modelsCache(§2.1 / §5.9)。
	snap := modelsCacheSnapshot()
	out := make([]*ModelInfo, 0, len(snap))
	for _, m := range snap {
		cp := *m
		out = append(out, &cp)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j-1].ID < out[j].ID {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// modelsSyncStamp 带锁读 lastSync(时间戳格式), 避免与同步协程的写形成数据竞争。
func modelsSyncStamp() string {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsLastSync.IsZero() {
		return ""
	}
	return modelsLastSync.UTC().Format(time.RFC3339)
}

type recommendedPayload struct {
	Free []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"free"`
}

func syncRecommendedModels() (int, error) {
	initModelsCache()

	acc := pickAccount()
	if acc == nil {
		return 0, fmt.Errorf("no active accounts")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("GET", recommendedModelsURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header = clineHeaders(token, "")
	req.Header.Set("X-Task-ID", fmt.Sprintf("sess_sync_%d", time.Now().UnixMilli()))

	// 走网关统一出口: 出口模式选节点时经节点出去, 直连模式才直连。
	ctx, cancel := context.WithTimeout(context.Background(), modelsSyncTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := getZenHTTPClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload recommendedPayload
	// 上游响应体封顶(2026-09-24 审计 P0-2: 出口是第三方节点, 响应字节不可信; 见 kit.DecodeJSONLimit)
	if err := kit.DecodeJSONLimit(resp.Body, &payload, kit.MaxUpstreamBodyBytes); err != nil {
		return 0, err
	}

	modelsMu.Lock()
	defer modelsMu.Unlock()

	added := 0
	for _, m := range payload.Free {
		id := m.ID
		provider := id
		if i := indexByte(id, '/'); i >= 0 {
			provider = id[:i]
		}
		if cached, ok := modelsCache[id]; ok {
			cached.Source = "free"
			cached.Cost = "free"
			cached.Provider = provider
			cached.Status = ModelActive
			cached.SyncedAt = time.Now()
			if cached.Name == "" {
				cached.Name = m.Name
			}
			continue
		}
		modelsCache[id] = &ModelInfo{
			ID:             id,
			Name:           m.Name,
			Source:         "free",
			Provider:       provider,
			Cost:           "free",
			Status:         ModelActive,
			RequiresStream: indexByte(id, ':') < 0,
			SyncedAt:       time.Now(),
		}
		added++
	}

	modelsLastSync = time.Now()
	return added, nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// syncModelsOnce 同步一次官方模型目录。返回的错误交给调用方记录,
// 这样后台刷新循环可以在连续失败时做刷屏抑制, 而按需路径(ensureModelsFresh)
// 仍照常打印每条失败 —— 同步逻辑本身(取锁/标记 syncing/调 syncRecommendedModels/
// 成功日志)一行未改。
func syncModelsOnce() error {
	initModelsCache()
	modelsMu.Lock()
	if modelsSyncing {
		modelsMu.Unlock()
		return nil
	}
	modelsSyncing = true
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsSyncing = false
		modelsMu.Unlock()
	}()

	added, err := syncRecommendedModels()
	if err != nil {
		return err
	}
	if added > 0 {
		log.Printf("  model sync: %d new free models from official feed", added)
	} else {
		log.Printf("  model sync: %d free models up to date", len(getFreeModels()))
	}
	return nil
}

func getDefaultModel() string {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if m, ok := modelsCache[defaultModel]; ok && m.Status == ModelActive {
		return defaultModel
	}
	// 兜底不能随机: Go map 遍历顺序随机, 同一缓存下每次可能返回不同模型,
	// 默认选路/日志不可复现。modelsCache 没有稳定顺序字段, 按 ID 取最小的
	// active(单趟最小值, 不引入每次全量排序的开销)。
	first := ""
	for _, m := range modelsCache {
		if m.Status != ModelActive {
			continue
		}
		if first == "" || m.ID < first {
			first = m.ID
		}
	}
	if first != "" {
		return first
	}
	return defaultModel
}

func normalizeRequestModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return getDefaultModel()
	}
	initModelsCache()
	modelsMu.Lock()
	_, ok := modelsCache[id]
	modelsMu.Unlock()
	if ok {
		return id
	}
	log.Printf("  model %q not in free list, fallback to %q", id, getDefaultModel())
	return getDefaultModel()
}

func apiModelList() []map[string]any {
	// 必须走持锁的 accessor, 不要裸读 modelsCache —— 那是全局 map, 与
	// syncRecommendedModels 的并发写构成数据竞争(本文件唯一一处裸读点)。
	// 顺便只取一次快照: 原来 len(modelsCache) 和下面的 range 是两次独立遍历。
	free := getFreeModels()
	out := make([]map[string]any, 0, len(free))
	for _, m := range free {
		out = append(out, map[string]any{
			"id":             m.ID,
			"object":         "model",
			"created":        time.Now().UnixMilli(),
			"owned_by":       m.Provider,
			"source":         m.Source,
			"status":         m.Status,
			"cost":           m.Cost,
			"requiresStream": m.RequiresStream,
			"syncedAt":       m.SyncedAt,
		})
		// OMP 下拉要选三段名 cline/cline-free/gemini-3.8-flash: 路由层 stripDisplayPrefix
		// 已支持剥离直通同一上游，这里只多发一条别名条目，不进 modelsCache。
		if m.ID == "cline-free/gemini-3.8-flash" {
			out = append(out, map[string]any{
				"id":             "cline/" + m.ID,
				"object":         "model",
				"created":        time.Now().UnixMilli(),
				"owned_by":       m.Provider,
				"source":         m.Source,
				"status":         m.Status,
				"cost":           m.Cost,
				"requiresStream": m.RequiresStream,
				"syncedAt":       m.SyncedAt,
			})
		}
	}
	return out
}

func ensureModelsFresh() {
	initModelsCache()
	modelsMu.Lock()
	needSync := modelsLastSync.IsZero() || time.Since(modelsLastSync) > modelsRefreshInterval
	syncing := modelsSyncing
	modelsMu.Unlock()
	if needSync && !syncing {
		go syncModelsOnce()
	}
}

func startModelsRefresher() {
	go func() {
		syncModelsOnce()
		ticker := time.NewTicker(modelsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				syncModelsOnce()
			case <-appRootCtx.Done():
				// 收到退出信号: 停止模型刷新协程, 让进程能够真正停下。
				return
			}
		}
	}()
}
