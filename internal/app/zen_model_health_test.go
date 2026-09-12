package app

import (
	"testing"
	"time"
)

// 需求: zen 模型连续硬失败(5xx/超时/网络/空响应, 非 429/4xx)达到阈值后,
// 网关将其视为暂不可用 —— 不再出现在模型列表, 请求快速失败而不是烧完 6 次重试。
// 成功一次即恢复计数; 不可用状态带 30 分钟过期, 到期后放行一次探测请求自愈。

func TestZenModelHealthTripsAfterConsecutiveHardFails(t *testing.T) {
	withZenModelsReset(t)
	resetZenModelHealth()
	initZenModels()
	for i := 0; i < zenModelFailThreshold; i++ {
		recordZenModelResult("mimo-v2.5-free", true)
	}
	if !zenModelUnavailable("mimo-v2.5-free") {
		t.Fatal("连续 5 次硬失败后模型必须被标为不可用")
	}
	if _, ok := resolveZenFreeModel("zen/mimo-v2.5-free"); ok {
		t.Fatal("不可用模型必须解析失败(快速失败)")
	}
}

func TestZenModelHealthSuccessResets(t *testing.T) {
	withZenModelsReset(t)
	resetZenModelHealth()
	initZenModels()
	for i := 0; i < zenModelFailThreshold-1; i++ {
		recordZenModelResult("mimo-v2.5-free", true)
	}
	recordZenModelResult("mimo-v2.5-free", false)
	recordZenModelResult("mimo-v2.5-free", true)
	if zenModelUnavailable("mimo-v2.5-free") {
		t.Fatal("中间成功一次必须清零计数, 不该被标不可用")
	}
}

func TestZenModelHealthExpiresAndReprobes(t *testing.T) {
	withZenModelsReset(t)
	resetZenModelHealth()
	oldHold := zenModelHoldMs
	zenModelHoldMs = int64(50)
	t.Cleanup(func() { zenModelHoldMs = oldHold })
	initZenModels()
	for i := 0; i < zenModelFailThreshold; i++ {
		recordZenModelResult("mimo-v2.5-free", true)
	}
	if !zenModelUnavailable("mimo-v2.5-free") {
		t.Fatal("前置条件: 必须先被标不可用")
	}
	time.Sleep(60 * time.Millisecond)
	if zenModelUnavailable("mimo-v2.5-free") {
		t.Fatal("过期后必须放行探测, 不能永久拉黑")
	}
	if _, ok := resolveZenFreeModel("zen/mimo-v2.5-free"); !ok {
		t.Fatal("过期后解析必须恢复")
	}
}

func TestZenFreeCatalogSkipsUnavailable(t *testing.T) {
	withZenModelsReset(t)
	resetZenModelHealth()
	initZenModels()
	for i := 0; i < zenModelFailThreshold; i++ {
		recordZenModelResult("mimo-v2.5-free", true)
	}
	for _, m := range zenFreeCatalog() {
		if m.ID == "mimo-v2.5-free" {
			t.Fatal("不可用模型不得出现在免费目录")
		}
	}
}

func TestZenModelHealthIgnoresNonZen(t *testing.T) {
	withZenModelsReset(t)
	resetZenModelHealth()
	recordZenModelResult("not-a-zen-model", true)
	recordZenModelResult("", true)
	if zenModelUnavailable("not-a-zen-model") {
		t.Fatal("目录外 ID 不得被跟踪")
	}
}
