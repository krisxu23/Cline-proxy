package app

import "testing"

func TestComboOrderStrategies(t *testing.T) {
	on := true
	c := &comboDef{
		Name:     "c1",
		Strategy: comboStrategyPriority,
		Targets: []comboTarget{
			{Upstream: "zen", Model: "a"},
			{Upstream: "zen", Model: "b", Enabled: &on},
			{Upstream: "zen", Model: "c", Enabled: &on},
		},
	}
	// priority: 声明顺序
	for i, want := range []string{"a", "b", "c"} {
		if got := comboOrder(c)[i].Model; got != want {
			t.Fatalf("priority 顺序错误: got %v", comboOrder(c))
		}
	}
	// 禁用的目标被剔除
	off := false
	c.Targets[1].Enabled = &off
	if got := comboOrder(c); len(got) != 2 || got[0].Model != "a" || got[1].Model != "c" {
		t.Fatalf("禁用目标应被剔除: %v", got)
	}
	c.Targets[1].Enabled = &on

	// round_robin: 两次调用打头者不同(旋转)
	c2 := &comboDef{Name: "c2", Strategy: comboStrategyRoundRobin, Targets: []comboTarget{
		{Upstream: "zen", Model: "a"}, {Upstream: "zen", Model: "b"}, {Upstream: "zen", Model: "c"},
	}}
	first := comboOrder(c2)[0].Model
	second := comboOrder(c2)[0].Model
	if first == second {
		t.Fatalf("round_robin 打头者应轮转, got %q 两次", first)
	}
	// 但都来自同一目标集合
	for _, m := range comboOrder(c2) {
		if m.Model != "a" && m.Model != "b" && m.Model != "c" {
			t.Fatalf("round_robin 不应产生新目标: %v", m)
		}
	}

	// weighted: 高权重打头概率显著更高(统计性断言)
	c3 := &comboDef{Name: "c3", Strategy: comboStrategyWeighted, Targets: []comboTarget{
		{Upstream: "zen", Model: "heavy", Weight: 100},
		{Upstream: "zen", Model: "light", Weight: 1},
	}}
	heavy := 0
	for i := 0; i < 200; i++ {
		if comboOrder(c3)[0].Model == "heavy" {
			heavy++
		}
	}
	if heavy < 150 {
		t.Fatalf("weight 100:1 时 heavy 打头应占绝大多数, got %d/200", heavy)
	}
}

func TestResolveComboAndExposure(t *testing.T) {
	// 注入一个组合, resolveRouteChain 应命中它并按顺序展开
	cfg := getZenConfig()
	backup := cfg.Combos
	defer func() { setZenConfig(cfg) }()

	next := cfg.clone()
	next.Combos = map[string]*comboDef{
		"test-combo": {Name: "test-combo", Strategy: comboStrategyPriority, Targets: []comboTarget{
			{Upstream: upstreamCline, Model: clinePoolPlaceholder},
		}},
	}
	setZenConfig(next)

	cands, matched, errMsg := resolveRouteChain("test-combo")
	if !matched || errMsg != "" {
		t.Fatalf("组合应被命中: matched=%v err=%s", matched, errMsg)
	}
	if len(cands) != 1 || cands[0].Upstream != upstreamCline || cands[0].Model != clinePoolPlaceholder {
		t.Fatalf("组合候选不符: %+v", cands)
	}
	// 组合名进别名列表(进而进 /v1/models)
	found := false
	for _, n := range routeAliasNames() {
		if n == "test-combo" {
			found = true
		}
	}
	if !found {
		t.Fatal("组合名应出现在 routeAliasNames(进而进 /v1/models)")
	}

	_ = backup // 恢复由 defer 完成
}

func TestValidateComboDef(t *testing.T) {
	if problems := validateComboDef(nil); len(problems) == 0 {
		t.Fatal("nil 定义应报错")
	}
	if problems := validateComboDef(&comboDef{Name: ""}); len(problems) == 0 {
		t.Fatal("空名字应报错")
	}
	if problems := validateComboDef(&comboDef{Name: "x", Strategy: "bogus"}); len(problems) == 0 {
		t.Fatal("未知策略应报错")
	}
	// cline 池目标必须用占位符
	if problems := validateComboDef(&comboDef{Name: "x", Targets: []comboTarget{{Upstream: upstreamCline, Model: "具体模型"}}}); len(problems) == 0 {
		t.Fatal("cline 非占位符应报错")
	}
}
