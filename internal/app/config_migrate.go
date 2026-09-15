package app

// 配置版本化与迁移 (P1-14, 参照 OmniRoute db/migrations 的演进纪律 —— 只学纪律,
// 不学 SQLite): 每个配置文件带 schemaVersion, 载入时按链式迁移函数逐版升级。
//
// 规则:
//   - 迁移函数必须是纯函数: 只改内存里的 cfg, 不做 IO;
//   - 每一版迁移都要有测试(见 config_migrate_test.go);
//   - 新版本号只增不减, 永不回写旧版本字段。

import (
	"log"
)

// zenConfigSchemaVersion 当前配置结构版本。
const zenConfigSchemaVersion = 1

// migrateZenConfig 把载入的配置迁移到当前版本。幂等: 已是最新版本时是 no-op。
// 返回是否发生了实际变更(调用方据此决定是否重新落盘)。
func migrateZenConfig(cfg *zenConfigData) bool {
	if cfg == nil {
		return false
	}
	changed := false
	for cfg.SchemaVersion < zenConfigSchemaVersion {
		next := cfg.SchemaVersion + 1
		switch next {
		case 1:
			// v1: 基线版本。补齐历史上散落在 defaults 里的字段
			// (旧配置可能只写了部分字段, 且 BaseURLs 出现之前的单 BaseURL)。
			if cfg.BaseURL != "" && len(cfg.BaseURLs) == 0 {
				cfg.BaseURLs = []string{cfg.BaseURL}
				changed = true
			}
			if cfg.SubsRefreshMins <= 0 {
				cfg.SubsRefreshMins = 30
				changed = true
			}
			if cfg.MaxConcurrency <= 0 {
				cfg.MaxConcurrency = 8
				changed = true
			}
			if cfg.Retries <= 0 {
				cfg.Retries = 3
				changed = true
			}
			if cfg.FailoverCount <= 0 {
				cfg.FailoverCount = 3
				changed = true
			}
			if cfg.FailoverMinutes <= 0 {
				cfg.FailoverMinutes = 5
				changed = true
			}
			if cfg.ProxyStrategy == "" {
				cfg.ProxyStrategy = "round_robin"
				changed = true
			}
			if cfg.ExitMode == "" {
				cfg.ExitMode = exitModeProxy
				changed = true
			}
			if cfg.RescueDirect == nil {
				tr := true
				cfg.RescueDirect = &tr
				changed = true
			}
		default:
			// 未知版本: 不该发生(zenConfigSchemaVersion 没跟上), 停止迁移并告警。
			log.Printf("  config: 未知的目标版本 %d, 停止迁移", next)
			return changed
		}
		cfg.SchemaVersion = next
		log.Printf("  config: schema 迁移到 v%d", next)
	}
	return changed
}
