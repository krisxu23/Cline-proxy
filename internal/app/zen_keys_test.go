package app

// zen 多 key 的测试。
//
// 动机: 一个 key 从上千个 IP 打是明显的代理特征; 按出口把 key 分组更接近正常
// 用户, 同时提供冗余。收益边界见 zen_keys.go 顶部说明。

import (
	"strings"
	"testing"
)

func TestZenAllKeys_去重与顺序(t *testing.T) {
	cfg := &zenConfigData{
		Key:  "k-main",
		Keys: []string{"k2", "", "  ", "k-main", "k3"},
	}
	got := zenAllKeys(cfg)
	want := []string{"k-main", "k2", "k3"}
	if len(got) != len(want) {
		t.Fatalf("zenAllKeys = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zenAllKeys = %v, 期望 %v (Key 字段应排在最前)", got, want)
		}
	}
	// 只有 Key 字段时也应返回一把
	if got := zenAllKeys(&zenConfigData{Key: "only"}); len(got) != 1 || got[0] != "only" {
		t.Fatalf("单 key 配置应返回 [only], 实得 %v", got)
	}
	// nil / 全空安全
	if got := zenAllKeys(nil); len(got) != 0 {
		t.Fatalf("nil 配置应返回空, 实得 %v", got)
	}
	if got := zenAllKeys(&zenConfigData{}); len(got) != 0 {
		t.Fatalf("空配置应返回空, 实得 %v", got)
	}
}

// 同一个出口必须永远映射到同一把 key —— 这是"配对稳定"的核心,
// 否则同一 key 会在多个 IP 之间乱跳, 反而更像代理。
func TestZenKeyForExit_确定性(t *testing.T) {
	keys := []string{"a", "b", "c"}
	for _, exit := range []string{"socks5://1.2.3.4:1080", "vmess://x", "http://5.6.7.8:8080"} {
		first := zenKeyForExit(keys, exit)
		for i := 0; i < 20; i++ {
			if got := zenKeyForExit(keys, exit); got != first {
				t.Fatalf("出口 %s 的 key 不稳定: %q vs %q", exit, first, got)
			}
		}
	}
}

// 多个出口应当分散到多把 key 上(而不是全挤在第一把)。
func TestZenKeyForExit_分散(t *testing.T) {
	keys := []string{"a", "b", "c"}
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		seen[zenKeyForExit(keys, "exit-"+string(rune('A'+i%26))+string(rune('0'+i%10))+string(rune('a'+i%26)))]++
	}
	if len(seen) < 2 {
		t.Fatalf("300 个不同出口应分散到多把 key, 实得只用 %d 把: %v", len(seen), seen)
	}
}

// 单 key 与直连(exitKey 为空)的行为。
func TestZenKeyForExit_退化情形(t *testing.T) {
	if got := zenKeyForExit([]string{"only"}, "whatever"); got != "only" {
		t.Fatalf("单 key 应返回它本身, 实得 %q", got)
	}
	if got := zenKeyForExit(nil, "x"); got != "" {
		t.Fatalf("无 key 应返回空, 实得 %q", got)
	}
	if got := zenKeyForExit([]string{"a", "b"}, ""); got != "a" {
		t.Fatalf("直连(无出口)应固定用第一把, 实得 %q", got)
	}
}

// 401 退役后, 该出口应自动落到别的 key。
func TestZenSelectKey_退役后换一把(t *testing.T) {
	zenClearRetiredKeysForTest()
	defer zenClearRetiredKeysForTest()

	cfg := &zenConfigData{Key: "a", Keys: []string{"b", "c"}}
	const exit = "socks5://1.2.3.4:1080"

	primary := zenSelectKey(cfg, exit)
	if primary == "" {
		t.Fatal("应选出一把 key")
	}
	zenRetireKey(primary)

	next := zenSelectKey(cfg, exit)
	if next == primary {
		t.Fatalf("退役的 key %q 不应再被选中", primary)
	}
	if next == "" {
		t.Fatal("退役后应回退到另一把 key, 而不是空")
	}
}

// 全部退役时仍要返回一把 —— 否则请求会不带凭据发出去, 那更糟。
func TestZenSelectKey_全退役仍返回(t *testing.T) {
	zenClearRetiredKeysForTest()
	defer zenClearRetiredKeysForTest()

	cfg := &zenConfigData{Key: "a", Keys: []string{"b"}}
	zenRetireKey("a")
	zenRetireKey("b")

	if got := zenSelectKey(cfg, "socks5://x:1"); got == "" {
		t.Fatal("全部退役时应回退到确定性选中的那把, 而不是返回空")
	}
}

// 掩码绝不能泄漏完整 key —— 日志经常被贴到 issue 里。
func TestMaskZenKey(t *testing.T) {
	const secret = "sk-rhSfeJfSGPyJr7w8bSRP559uKrSbserUteDlkXn35kaWMFj3jGDj1fB8r1pYOI"
	masked := maskZenKey(secret)
	if strings.Contains(masked, secret) {
		t.Fatal("掩码不得包含完整 key")
	}
	if !strings.Contains(masked, "[REDACTED]") {
		t.Fatalf("掩码应带 [REDACTED] 标记, 实得 %q", masked)
	}
	// 短 key 整体隐去
	if got := maskZenKey("short"); got != "[REDACTED]" {
		t.Fatalf("短 key 应整体隐去, 实得 %q", got)
	}
	if got := maskZenKey(""); got != "(empty)" {
		t.Fatalf("空 key 应返回 (empty), 实得 %q", got)
	}
}

// 匿名模式凭据选择: 免费模型统一 public(与出口无关、不参与 key 哈希与退役),
// 匿名关或非免费模型回退到按出口确定性选 key。
func TestZenSelectKeyForModel_匿名与回退(t *testing.T) {
	zenClearRetiredKeysForTest()
	defer zenClearRetiredKeysForTest()

	keys := []string{"k-main", "k2"}
	cfg := &zenConfigData{Key: "k-main", Keys: []string{"k2"}, Anonymous: true}
	exits := []string{"", "socks5://1.2.3.4:1080", "vmess://x"}

	// 匿名开 + 免费模型(-free 后缀判定, 不依赖目录) → 任何出口都是 public
	for _, exit := range exits {
		if got := zenSelectKeyForModel(cfg, exit, "mimo-v2.5-free"); got != zenAnonymousCredential {
			t.Fatalf("匿名+免费 exit=%q: 期望 %q, 实得 %q", exit, zenAnonymousCredential, got)
		}
	}

	// 匿名关 → 免费模型也走 key 选择(按出口确定性)
	cfg.Anonymous = false
	for _, exit := range exits {
		want := zenKeyForExit(keys, exit)
		if got := zenSelectKeyForModel(cfg, exit, "mimo-v2.5-free"); got != want {
			t.Fatalf("匿名关 exit=%q: 期望 %q, 实得 %q", exit, want, got)
		}
	}

	// 匿名开 + 未知(非免费)模型 → 仍走 key, 付费路径不能被打成 public
	cfg.Anonymous = true
	for _, exit := range exits {
		want := zenKeyForExit(keys, exit)
		if got := zenSelectKeyForModel(cfg, exit, "totally-unknown-model"); got != want {
			t.Fatalf("匿名+未知模型 exit=%q: 期望 %q, 实得 %q", exit, want, got)
		}
	}

	// nil 配置安全: 退化为 zenSelectKey(nil) → 空
	if got := zenSelectKeyForModel(nil, "x", "mimo-v2.5-free"); got != "" {
		t.Fatalf("nil 配置应返回空, 实得 %q", got)
	}
}
