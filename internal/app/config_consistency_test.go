package app

// 配置一致性门禁 (P1-15, 参照 OmniRoute 的 check-provider-consistency):
// 配置里的引用必须是"真实存在的上游+模型", 否则静默跳过/错误回落。
// 白名单不需要 —— 我们直接用可构造的 fixture 断言校验器的判定。

import (
	"strings"
	"testing"
)

func TestMigrateZenConfigBaseline(t *testing.T) {
	// v0 老配置: 只有 baseURL, 其余全空 → 应补齐默认值并升到当前版本
	cfg := &zenConfigData{Enabled: true, Key: "k", BaseURL: "https://example.com/v1", BaseURLs: nil}
	changed := migrateZenConfig(cfg)
	if !changed {
		t.Fatal("v0 配置应发生迁移")
	}
	if cfg.SchemaVersion != zenConfigSchemaVersion {
		t.Fatalf("应升到 v%d, got v%d", zenConfigSchemaVersion, cfg.SchemaVersion)
	}
	if len(cfg.BaseURLs) != 1 || cfg.BaseURLs[0] != "https://example.com/v1" {
		t.Fatalf("BaseURL 应并入 BaseURLs, got %v", cfg.BaseURLs)
	}
	if cfg.SubsRefreshMins != 30 || cfg.MaxConcurrency != 8 || cfg.Retries != 3 {
		t.Fatalf("默认值应补齐: %+v", cfg)
	}
	if cfg.RescueDirect == nil || !*cfg.RescueDirect {
		t.Fatal("RescueDirect 缺省应为 true")
	}

	// 幂等: 再迁移一次不应有变更
	if changed := migrateZenConfig(cfg); changed {
		t.Fatal("已是最新版本时迁移应为 no-op")
	}
	// nil 安全
	if migrateZenConfig(nil) {
		t.Fatal("nil 配置应返回 false")
	}
}

func TestValidateRouteTargets(t *testing.T) {
	// 注意: 测试进程里 zen 模型目录是运行时缓存, clinepass/provider 目录依赖配置;
	// 这里用"可确定性判定"的三类断言:
	//   1. 未配置的 provider → 必须报
	//   2. cline 池写了非占位符 → 必须报
	//   3. zen 不接受占位符 → 必须报
	//   4. 空链/合法形态 → 不报
	cfg := &zenConfigData{
		Routes: map[string][]string{
			"bad-provider": {"nosuch:v1"},
			"bad-cline":    {"cline:具体模型"},
			"bad-zen":      {"zen:*"},
			"ok-shape":     {"cline:" + clinePoolPlaceholder},
			"empty":        {},
		},
	}
	problems := validateRouteTargets(cfg)
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "供应商 nosuch 未配置") {
		t.Fatalf("未配置的 provider 应被告警, got: %s", joined)
	}
	if !strings.Contains(joined, "cline 池只支持占位符") {
		t.Fatalf("cline 非占位符应被告警, got: %s", joined)
	}
	if !strings.Contains(joined, "zen 不支持占位符") {
		t.Fatalf("zen 占位符应被告警, got: %s", joined)
	}
	if strings.Contains(joined, "ok-shape") || strings.Contains(joined, "empty") {
		t.Fatalf("合法形态不应被告警, got: %s", joined)
	}
	if cfg := (&zenConfigData{}); len(validateRouteTargets(cfg)) != 0 {
		t.Fatal("无路由配置应零告警")
	}
	if validateRouteTargets(nil) != nil {
		t.Fatal("nil 配置应零告警")
	}
}

func TestRouteAliasExposedInModels(t *testing.T) {
	// P0-5 的门禁: 只要配置了别名, /v1/models 聚合里就必须能看到它
	// (routeAliasNames 曾是死代码, 用这个测试锁住"必须暴露")。
	decisionTraceReset()
	defer decisionTraceReset()
	names := routeAliasNames()
	if len(names) == 0 {
		t.Fatal("默认配置下至少应有自动路由别名")
	}
	models := routeAliasModels()
	if len(models) < len(names) {
		t.Fatalf("别名模型条数(%d)应 >= 别名数(%d)", len(models), len(names))
	}
	seen := map[string]bool{}
	for _, m := range models {
		id, _ := m["id"].(string)
		seen[id] = true
		if m["owned_by"] != "combo" {
			t.Fatalf("别名 %s 的 owned_by 应为 combo, got %v", id, m["owned_by"])
		}
	}
	for _, n := range names {
		if !seen[n] {
			t.Fatalf("别名 %s 未暴露进模型目录", n)
		}
	}
}
