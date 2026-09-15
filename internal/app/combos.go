package app

// 组合模型 (P1-13, 参照 OmniRoute 的 combos + "策略只重排候选、不早退"):
// 一个对外可见的模型名绑定多个 (upstream, model) 目标, 网关按策略排序后
// 逐站尝试 —— 完全复用既有候选链的"首字节前 failover + 冷却 + 记账"。
//
// 设计取舍(刻意保持最小):
//   - 组合名与真实模型名是两个命名空间, 组合名优先解析(允许故意同名遮蔽,
//     与 OmniRoute 的 COMBO_NAME_SHADOWS_MODEL 一致, 这里先不做告警);
//   - 策略只决定候选顺序, 不改变调度语义 —— priority/round_robin/weighted
//     三种已覆盖"顺序优先 / 均摊 / 加权优先"三类真实需求;
//   - 配额与健康不进策略: candidateSkip 已经统一处理冷却、日限额与模型健康,
//     策略层重复实现只会造成两套真相。

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
)

// comboStrategy 组合模型的排序策略。
const (
	comboStrategyPriority   = "priority"    // 按声明顺序(默认)
	comboStrategyRoundRobin = "round_robin" // 轮流打头, 均摊用量
	comboStrategyWeighted   = "weighted"    // 按 weight 加权打头, 其余按权重降序兜底
)

// comboTarget 组合里的一个目标站。
type comboTarget struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
	Weight   int    `json:"weight,omitempty"` // weighted 策略用; 0 视为 1
	Enabled  *bool  `json:"enabled,omitempty"`
}

// comboDef 组合模型定义。
type comboDef struct {
	Name     string        `json:"name"`
	Strategy string        `json:"strategy,omitempty"`
	Targets  []comboTarget `json:"targets"`
}

// comboCounters round_robin 的轮转计数器: 组名 -> 已轮转次数。
// 单进程内互斥保护即可(每请求 bump 一次, 无竞争压力)。
var (
	comboCounterMu sync.Mutex
	comboCounters  = map[string]uint64{}
)

func comboCounterBump(name string) uint64 {
	comboCounterMu.Lock()
	defer comboCounterMu.Unlock()
	comboCounters[name]++
	return comboCounters[name]
}

// comboEnabled 目标是否启用(nil 视为启用)。
func (t comboTarget) comboEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// comboWeight 加权用的权重(下限 1, 防止除零)。
func (t comboTarget) comboWeight() int {
	if t.Weight < 1 {
		return 1
	}
	return t.Weight
}

// validateComboDef 组合定义的静态校验; 返回问题列表(空=通过)。
func validateComboDef(c *comboDef) []string {
	var problems []string
	if c == nil {
		return []string{"组合定义为空"}
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		problems = append(problems, "组合名不能为空")
	}
	if strings.ContainsAny(name, " \t\r\n") {
		problems = append(problems, "组合名不能包含空白字符: "+name)
	}
	switch c.Strategy {
	case "", comboStrategyPriority, comboStrategyRoundRobin, comboStrategyWeighted:
	default:
		problems = append(problems, fmt.Sprintf("未知策略 %q (支持 priority / round_robin / weighted)", c.Strategy))
	}
	if len(c.Targets) == 0 {
		problems = append(problems, "组合至少需要一个目标")
		return problems
	}
	if len(c.Targets) > 16 {
		problems = append(problems, "组合目标过多(上限 16)")
	}
	cfg := getZenConfig()
	for i, tg := range c.Targets {
		up := strings.TrimSpace(tg.Upstream)
		model := strings.TrimSpace(tg.Model)
		if up == "" || model == "" {
			problems = append(problems, fmt.Sprintf("目标 #%d 的 upstream/model 不能为空", i+1))
			continue
		}
		cand := routeCandidate{Upstream: up, Model: model}
		switch up {
		case upstreamZen:
			if _, ok := resolveZenModel(model); !ok {
				problems = append(problems, fmt.Sprintf("目标 #%d: zen 目录里没有模型 %s", i+1, model))
			}
		case upstreamCline:
			if model != clinePoolPlaceholder {
				problems = append(problems, fmt.Sprintf("目标 #%d: cline 池只支持占位符 %s", i+1, clinePoolPlaceholder))
			}
		case upstreamClinePass:
			if _, ok := clinePassModelByID(model); !ok {
				problems = append(problems, fmt.Sprintf("目标 #%d: ClinePass 目录里没有模型 %s", i+1, model))
			}
		default:
			if cfg != nil {
				if _, ok := cfg.Providers[up]; !ok {
					problems = append(problems, fmt.Sprintf("目标 #%d: 供应商 %s 未配置", i+1, up))
					continue
				}
			}
			if p := providerByName(up); p != nil {
				found := false
				for _, fm := range freeModelsFor(up, p) {
					if fm.ID == model {
						found = true
						break
					}
				}
				if !found {
					problems = append(problems, fmt.Sprintf("目标 #%d: 供应商 %s 目录里没有模型 %s", i+1, up, model))
				}
			}
		}
		_ = cand
	}
	return problems
}

// comboOrder 按策略对启用的目标排序。返回的顺序即候选顺序 —— 调度层
// 之后照常逐站尝试, 失败换下一站, 策略层不早退。
func comboOrder(c *comboDef) []comboTarget {
	enabled := make([]comboTarget, 0, len(c.Targets))
	for _, t := range c.Targets {
		if t.comboEnabled() {
			enabled = append(enabled, t)
		}
	}
	if len(enabled) <= 1 {
		return enabled
	}
	switch c.Strategy {
	case comboStrategyRoundRobin:
		// 轮流打头: 用组合名计数器旋转, 均摊用量
		n := comboCounterBump("combo:" + c.Name)
		off := int(n % uint64(len(enabled)))
		out := make([]comboTarget, 0, len(enabled))
		out = append(out, enabled[off:]...)
		out = append(out, enabled[:off]...)
		return out
	case comboStrategyWeighted:
		// 加权抽一个打头(权重越大越常打头), 其余按权重降序兜底
		total := 0
		for _, t := range enabled {
			total += t.comboWeight()
		}
		pick, rest := 0, make([]comboTarget, 0, len(enabled))
		if total > 0 {
			r := rand.Intn(total)
			for i, t := range enabled {
				r -= t.comboWeight()
				if r < 0 {
					pick = i
					rest = append(rest, enabled[:i]...)
					rest = append(rest, enabled[i+1:]...)
					break
				}
			}
		}
		if len(rest) == 0 { // 理论不可达, 防御
			return enabled
		}
		// rest 按权重降序
		for i := 1; i < len(rest); i++ {
			for j := i; j > 0 && rest[j].comboWeight() > rest[j-1].comboWeight(); j-- {
				rest[j], rest[j-1] = rest[j-1], rest[j]
			}
		}
		out := make([]comboTarget, 0, len(enabled))
		out = append(out, enabled[pick])
		out = append(out, rest...)
		return out
	default: // priority: 声明顺序
		return enabled
	}
}

// resolveCombo 组合名 -> 有序候选。第二个返回值表示是否命中组合。
func resolveCombo(name string) ([]routeCandidate, bool) {
	cfg := getZenConfig()
	if cfg == nil || len(cfg.Combos) == 0 {
		return nil, false
	}
	c, ok := cfg.Combos[strings.TrimSpace(name)]
	if !ok || c == nil || len(c.Targets) == 0 {
		return nil, false
	}
	targets := comboOrder(c)
	out := make([]routeCandidate, 0, len(targets))
	for _, t := range targets {
		out = append(out, routeCandidate{Upstream: strings.TrimSpace(t.Upstream), Model: strings.TrimSpace(t.Model)})
	}
	return out, true
}

// comboNames 当前配置里的组合名(排序后)。
func comboNames() []string {
	cfg := getZenConfig()
	if cfg == nil || len(cfg.Combos) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.Combos))
	for k := range cfg.Combos {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // 插入排序, 少量元素无需引入 sort 依赖
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
