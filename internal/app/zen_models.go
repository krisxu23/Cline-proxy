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

	// tryAll 用给定客户端把全部端点试一遍。抽成闭包是为了让"经出口"与"直连兜底"
	// 走同一段逻辑(否则两处的响应处理迟早会漂移)。
	tryAll := func(client *http.Client) (int, error) {
		var err error
		for _, base := range zenBaseURLList(cfg) {
			endpoint := base + "/models"
			req, rerr := http.NewRequest("GET", endpoint, nil)
			if rerr != nil {
				return 0, rerr
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Key)
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			resp, derr := client.Do(req.WithContext(ctx))
			cancel()
			if derr != nil {
				err = derr
				continue
			}
			if resp.StatusCode != 200 {
				body := kit.ReadBody(resp)
				resp.Body.Close()
				err = fmt.Errorf("%s HTTP %d: %s", base, resp.StatusCode, kit.Truncate(body, 200))
				continue
			}
			added, derr2 := decodeZenModels(resp)
			if derr2 != nil {
				err = derr2
				continue
			}
			return added, nil
		}
		if err == nil {
			err = fmt.Errorf("no zen endpoints configured")
		}
		return 0, err
	}

	added, err := tryAll(getZenHTTPClient())
	if err == nil {
		return added, nil
	}
	lastErr = err

	// 直连兜底: 目录同步是**控制面**请求 —— 出口池整体失效时(实测 exitReachable
	// 38/4482), 若这里直接放弃, 面板就一直吃历史缓存, 用户看到的是"永远拉取不了
	// 上游免费模型"。目标站点直连通常可达(实测 1.3s 200), 所以值得再试一遍。
	if rescueDirectEnabled() {
		log.Printf("  zen: 目录经出口全部失败(%v), 走直连兜底再试一次", err)
		added, derr := tryAll(directHTTPClient())
		if derr == nil {
			log.Printf("  zen: 目录直连兜底成功(同步到 %d 个模型)", added)
			return added, nil
		}
		lastErr = fmt.Errorf("%v; 直连兜底也失败: %v", err, derr)
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
	// 空目录不当作"成功同步": 既不做差量删除(上游结构变化/权限问题时, 一次
	// 空响应不该把整个 synced 目录清空), 也不刷新缓存 SyncedAt —— 目录不可用
	// 不配推进 7 天 TTL(P2-16)。
	if len(payload.Data) == 0 {
		log.Printf("  zen: 目录同步返回空列表, 保留现有模型与缓存时间戳")
		return 0, nil
	}

	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	added := 0
	present := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		id := item.ID
		if id == "" {
			continue
		}
		present[id] = true
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
	// 差量删除(P2-16): 本次全量目录里已经没有的 synced 条目视为上游下架,
	// 从内存目录里摘除 —— 否则模型目录只增不删, 死模型跨重启永久驻留, 上游
	// 对其回 400/404 时不计健康门(只记 5xx), 每次请求白走全链路后报错。
	// 只动 synced 来源: seed 条目归目录初始化管, 不由上游目录决定。
	removed := 0
	for id, m := range zenModels {
		if m == nil || m.Source != "synced" {
			continue
		}
		if present[id] {
			continue
		}
		delete(zenModels, id)
		removed++
	}
	if removed > 0 {
		log.Printf("  zen: 目录同步摘除 %d 个已下架的模型", removed)
	}
	// 真正成功同步且目录可用 → 刷新 SyncedAt 并重写缓存(含上面的差量删除)。
	saveZenModelsCacheSyncedLocked()
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
