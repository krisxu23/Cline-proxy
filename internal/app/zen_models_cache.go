package app

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"cline-go-proxy/internal/kit"
)

// zen 模型目录持久化。
//
// zenModels 里 "synced" 来源的模型(官方 /models 接口同步来的)落盘
// data/zen-models-cache.json, 启动时恢复。没有这份缓存时, 每次重启后第一次
// zen 目录同步失败(节点抖动很常见, 实测出现过)会让之前同步到的模型全部消失,
// 用户正在使用的模型被误报成 "is a paid zen model" —— 其实只是目录里暂时没有。
//
// 缓存条目带时间戳, 超过 7 天视为过期(上游下架的模型不该永久驻留)。

func zenModelsCachePath() string { return kit.ResolveDataPath("zen-models-cache.json") }

type zenModelsCacheFile struct {
	SyncedAt int64      `json:"syncedAt"`
	Models   []ZenModel `json:"models"`
}

const zenModelsCacheTTL = 7 * 24 * time.Hour

// saveZenModelsCacheLocked 落盘当前 synced 来源的模型。调用方持有 zenModelsMu。
func saveZenModelsCacheLocked() {
	models := make([]ZenModel, 0, len(zenModels))
	for _, m := range zenModels {
		if m != nil && m.Source == "synced" {
			models = append(models, *m)
		}
	}
	if len(models) == 0 {
		return
	}
	payload, err := json.Marshal(zenModelsCacheFile{SyncedAt: time.Now().Unix(), Models: models})
	if err != nil {
		return
	}
	path := zenModelsCachePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		log.Printf("  zen: 写模型缓存失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		log.Printf("  zen: 模型缓存改名失败: %v", err)
	}
}

// loadZenModelsCache 恢复上次同步到的模型, 与种子合并。调用方持有 zenModelsMu。
func loadZenModelsCache() {
	raw, err := os.ReadFile(zenModelsCachePath())
	if err != nil {
		return // 不存在 = 首次运行, 正常
	}
	var cf zenModelsCacheFile
	if json.Unmarshal(raw, &cf) != nil || len(cf.Models) == 0 {
		return
	}
	if time.Since(time.Unix(cf.SyncedAt, 0)) > zenModelsCacheTTL {
		log.Printf("  zen: 模型缓存已过期(%d 天前), 等待下次同步", int(time.Since(time.Unix(cf.SyncedAt, 0)).Hours()/24))
		return
	}
	restored := 0
	for _, m := range cf.Models {
		if m.ID == "" {
			continue
		}
		if _, ok := zenModels[m.ID]; ok {
			continue
		}
		if _, conflict := zenAliases[m.ID]; conflict {
			continue // 与免费模型别名冲突的 ID 不恢复, 保证别名解析不被覆盖
		}
		cp := m
		cp.Source = "synced"
		zenModels[cp.ID] = &cp
		restored++
	}
	if restored > 0 {
		log.Printf("  zen: 从缓存恢复 %d 个上次同步的模型(目录同步尚未成功)", restored)
	}
}
