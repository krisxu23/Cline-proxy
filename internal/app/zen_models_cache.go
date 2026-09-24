package app

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"free-router/internal/kit"
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
//
// 这里**不刷新** SyncedAt: 它表示"最后一次成功同步全量目录"的时间, 只有
// decodeZenModels(真正成功同步且目录可用)才配推进它 —— 每次落盘都无脑刷新
// 会把 7 天 TTL 一路往后推, 过期淘汰永远不触发, 上游下架的模型跨重启永久
// 驻留(与本文件头注释矛盾, P2-16)。无历史时间戳(首次写入)时取当前时间。
func saveZenModelsCacheLocked() {
	saveZenModelsCacheWithSyncedAtLocked(false)
}

// saveZenModelsCacheSyncedLocked 与 saveZenModelsCacheLocked 相同, 但把 SyncedAt
// 刷新为当前时间 —— 只供"真正成功同步了全量目录"的路径调用(见 P2-16)。
func saveZenModelsCacheSyncedLocked() {
	saveZenModelsCacheWithSyncedAtLocked(true)
}

// saveZenModelsCacheWithSyncedAtLocked 落盘公共实现。调用方持有 zenModelsMu。
func saveZenModelsCacheWithSyncedAtLocked(refresh bool) {
	models := make([]ZenModel, 0, len(zenModels))
	for _, m := range zenModels {
		if m != nil && m.Source == "synced" {
			models = append(models, *m)
		}
	}
	if len(models) == 0 && !refresh {
		// 非同步路径没有"上游目录可用"的背书, 空列表照写会把磁盘缓存清空
		// (首次运行/尚未同步场景的原保护)。同步路径(refresh)不受此限:
		// 空 payload 在 decodeZenModels 已被拦下, 能走到这里只可能是差量
		// 删除摘光了 synced 条目 —— 必须写空列表, 否则重启后已下架的模型
		// 从旧缓存复活, 抵消差量删除(终审 P3)。
		return
	}
	syncedAt := time.Now().Unix()
	if !refresh {
		// 保留文件里已有的时间戳; 没有(首次写入)才用当前时间。
		if old, ok := readZenModelsCacheSyncedAt(); ok {
			syncedAt = old
		}
	}
	payload, err := json.Marshal(zenModelsCacheFile{SyncedAt: syncedAt, Models: models})
	if err != nil {
		return
	}
	path := zenModelsCachePath()
	// 原子写(临时文件 + fsync + rename), 与全仓其它落盘点同一口径。
	//
	// 原实现自己拼 `path + ".tmp"` 再 os.Rename: 固定 .tmp 名在并发写时互踩,
	// 且无 fsync —— 强杀中途会留半截 JSON, 下次 loadZenModelsCache 解析失败后
	// **静默丢缓存**(2026-09-24 审查)。kit.WriteFileAtomicDefault 已把这些都做掉。
	if err := kit.WriteFileAtomicDefault(path, payload); err != nil {
		log.Printf("  zen: 写模型缓存失败: %v", err)
	}
}

// readZenModelsCacheSyncedAt 读缓存文件里已有的 SyncedAt(不存在/损坏/为 0 返回 false)。
func readZenModelsCacheSyncedAt() (int64, bool) {
	raw, err := os.ReadFile(zenModelsCachePath())
	if err != nil {
		return 0, false
	}
	var cf zenModelsCacheFile
	if json.Unmarshal(raw, &cf) != nil || cf.SyncedAt <= 0 {
		return 0, false
	}
	return cf.SyncedAt, true
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
