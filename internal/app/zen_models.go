package app

import (
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func zenModelList() []map[string]any {
	initZenModels()
	zenModelsMu.RLock()
	out := make([]map[string]any, 0, len(zenModels))
	for _, m := range zenModels {
		if !isZenFreeModel(m) || zenModelUnavailable(m.ID) {
			continue
		}
		cp := *m
		out = append(out, map[string]any{
			"id":      "zen/" + cp.ID,
			"context": cp.Context,
			"output":  cp.Output,
			"source":  cp.Source,
		})
	}
	zenModelsMu.RUnlock()
	return out
}

// syncZenModels 拉取 zen /v1/models,动态合并到模型表。
// 逐端点尝试: 官方地址失败后依次落 CDN 镜像。
func syncZenModels() (int, error) {
	initZenModels()
	cfg := getZenConfig()
	var lastErr error
	for _, base := range zenBaseURLList(cfg) {
		endpoint := base + "/models"
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
		// 走网关统一出口(与 zen 对话同一链路), 出口模式跟随全局直连/节点选择
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		resp, err := getZenHTTPClient().Do(req.WithContext(ctx))
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			body := kit.ReadBody(resp)
			resp.Body.Close()
			lastErr = fmt.Errorf("%s HTTP %d: %s", base, resp.StatusCode, kit.Truncate(body, 200))
			continue
		}
		added, err := decodeZenModels(resp)
		if err != nil {
			lastErr = err
			continue
		}
		return added, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no zen endpoints configured")
	}
	return 0, lastErr
}

// decodeZenModels 解析 /models 响应并合并进模型表。
func decodeZenModels(resp *http.Response) (int, error) {
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	added := 0
	for _, item := range payload.Data {
		id := item.ID
		if id == "" {
			continue
		}
		if _, ok := zenModels[id]; ok {
			continue
		}
		// 跳过与免费模型别名冲突的 ID(如付费的 deepseek-v4-flash),保证别名解析不被覆盖
		if _, conflict := zenAliases[id]; conflict {
			continue
		}
		// 新模型:默认按 200K 上下文接入,输出按 32K
		zenModels[id] = &ZenModel{
			ID:      id,
			Context: 200000,
			Output:  32768,
			Source:  "synced",
		}
		added++
	}
	if added > 0 {
		saveZenModelsCacheLocked()
	}
	return added, nil
}

// startZenModelsRefresher 定时同步 zen 模型列表(成功后每 10 分钟;
// 启动后第一次成功之前每 1 分钟重试 —— 目录里的同步模型关系到
// 用户正在使用的模型能否被解析, 不能让一次节点抖动卡 10 分钟)。
func startZenModelsRefresher() {
	go func() {
		for {
			if _, err := syncZenModels(); err == nil {
				break
			} else {
				log.Printf("zen model sync: failed (%v), retrying in 1m (seed + cached models in use)", err)
			}
			time.Sleep(time.Minute)
		}
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cfg := getZenConfig()
				if !cfg.Enabled {
					continue
				}
				if added, err := syncZenModels(); err != nil {
					log.Printf("zen model sync: failed (%v)", err)
				} else if added > 0 {
					log.Printf("zen model sync: %d new models from official feed", added)
				}
			case <-appRootCtx.Done():
				// 收到退出信号: 停止模型同步协程, 让进程能够真正停下。
				return
			}
		}
	}()
}
