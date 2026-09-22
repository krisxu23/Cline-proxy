package app

// zen 多 key 支持。
//
// 动机(用户提出): 一个 key 配一个出口子集, 比"一个 key 从上千个 IP 打"更像
// 正常用户, 更不容易被风控识别; 另外某个 key 被 ban 时还有别的 key 可用。
//
// ★ 诚实说明收益边界(写在代码里, 免得后人误以为多 key 是银弹):
//   - **确定的收益是冗余性**: 一个 key 收到 401 时, 其他 key 照常工作。
//   - **隐蔽性收益取决于 key 数量**: 若只有 2-3 个 key 而出口有上千个, 每个 key
//     仍然会从数百个 IP 打 —— 与单 key 相比改善有限。要让"一个 key 一个 IP"真正
//     成立, 需要与出口同量级的 key 数。
//
// 配对方式: 按出口标识**确定性哈希**到 key, 而不是每次随机挑。同一个出口永远
// 用同一个 key, (key, 出口) 的对应关系稳定 —— 不会出现"同一 key 在多个 IP 之间
// 乱跳"的模式, 这比随机轮换更接近正常用户的网络特征。
//
// 负向处理: 某 key 收到 401(认证失败)时把它临时退役, 受影响出口自动落到下一个
// key。退役状态**不落盘** —— 上游随时可能恢复(与 zen_responses.go 的负向纪律一致)。

import (
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

const (
	// zenKeyRetireDuration 401 后该 key 的退役时长。
	// 取 10 分钟: 401 多为 key 被临时吊销或额度冻结, 太久会浪费可用凭据;
	// 太短则会反复撞同一个坏 key。
	zenKeyRetireDuration = 10 * time.Minute
)

var (
	zenKeyStateMu sync.Mutex
	zenKeyRetired = map[string]time.Time{} // key -> 退役截止(仅进程内)
)

// zenAllKeys 该配置下的全部可用 key。
//
// 顺序: Key(兼容旧配置的单 key 字段)在前, 其余按 Keys 的声明顺序。
// 去重且丢弃空白项 —— 重复 key 会让哈希分布偏斜。
func zenAllKeys(cfg *zenConfigData) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 1+len(cfg.Keys))
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	add(cfg.Key)
	for _, k := range cfg.Keys {
		add(k)
	}
	return out
}

// zenKeyForExit 按出口标识确定性选取 key。
//
// 同一个出口永远映射到同一个 key(只要 key 集合不变)。exitKey 为空(直连)时
// 取第一个 key —— 直连没有出口可绑定, 固定用一个即可。
func zenKeyForExit(keys []string, exitKey string) string {
	if len(keys) == 0 {
		return ""
	}
	if len(keys) == 1 || exitKey == "" {
		return keys[0]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(exitKey))
	return keys[int(h.Sum32()%uint32(len(keys)))]
}

// zenSelectKey 本次请求该用哪把 key。
//
// 先按出口确定性选; 若选中的 key 正处于退役期, 则沿 keys 顺序找下一把可用的。
// 全部退役时回退到确定性选中的那把(总比不带凭据好, 而且退役只是猜测)。
func zenSelectKey(cfg *zenConfigData, exitKey string) string {
	keys := zenAllKeys(cfg)
	if len(keys) == 0 {
		return ""
	}
	primary := zenKeyForExit(keys, exitKey)
	if !zenKeyIsRetired(primary) {
		return primary
	}
	start := 0
	for i, k := range keys {
		if k == primary {
			start = i
			break
		}
	}
	for i := 1; i < len(keys); i++ {
		cand := keys[(start+i)%len(keys)]
		if !zenKeyIsRetired(cand) {
			return cand
		}
	}
	return primary // 全退役: 仍然用它, 否则请求会裸奔
}

func zenKeyIsRetired(key string) bool {
	if key == "" {
		return false
	}
	zenKeyStateMu.Lock()
	defer zenKeyStateMu.Unlock()
	until, ok := zenKeyRetired[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(zenKeyRetired, key)
		return false
	}
	return true
}

// zenRetireKey 某 key 收到 401 → 临时退役, 受影响出口自动落到下一个 key。
func zenRetireKey(key string) {
	if key == "" {
		return
	}
	zenKeyStateMu.Lock()
	defer zenKeyStateMu.Unlock()
	zenKeyRetired[key] = time.Now().Add(zenKeyRetireDuration)
}

// maskZenKey 日志用的 key 掩码: 保留前 4 + 后 4, 中间 [REDACTED]。
//
// 绝不把完整 key 写进日志 —— 日志经常被贴到 issue / 群里, 等于泄漏凭据。
func maskZenKey(key string) string {
	if key == "" {
		return "(empty)"
	}
	if len(key) <= 8 {
		return "[REDACTED]"
	}
	return key[:4] + "[REDACTED]" + key[len(key)-4:]
}

// zenClearRetiredKeysForTest 测试隔离用。
func zenClearRetiredKeysForTest() {
	zenKeyStateMu.Lock()
	zenKeyRetired = map[string]time.Time{}
	zenKeyStateMu.Unlock()
}

// zenAnonymousCredential opencode 匿名档的公开凭据。
//
// 2026-09-22 直连探针(同一 free-shape body, 只换 Authorization):
//
//	缺 Authorization / Bearer public → 403 FreeTierError(已进匿名通道, 只被出口 IP 门禁挡)
//	伪造 key / 随机 UUID             → 401 AuthError: Invalid API key.
//
// 即 "public" 是上游公开的匿名凭据, 与 opencode2api README 一致
// ("使用 OpenCode public 凭证的可选 Zen 匿名通道 / 匿名请求在上游认证头中使用 public 凭证")。
const zenAnonymousCredential = "public"

// zenSelectKeyForModel 本次请求发给上游的凭据。
//
// 匿名模式开 && 模型属免费层 → 统一 zenAnonymousCredential("public"),
// 不消耗配置 key、不参与出口哈希与 401 退役 —— 这就是匿名档:
// 所有出口共用一个公开凭据, 故障转移只按出口冷却(与 opencode2api 的
// anonymousPool 同构: public 凭据按代理独立冷却、绝不换绑凭据)。
//
// 其余情况(匿名关 / 付费或未知模型)走 zenSelectKey 的按出口确定性选 key。
// 免费层判定与形态整形共用 zenFreeModelEligible 一个口径。
func zenSelectKeyForModel(cfg *zenConfigData, exitKey, modelID string) string {
	if cfg != nil && cfg.Anonymous && zenFreeModelEligible(modelID) {
		return zenAnonymousCredential
	}
	return zenSelectKey(cfg, exitKey)
}
