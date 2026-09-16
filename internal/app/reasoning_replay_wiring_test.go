package app

import (
	"os"
	"strings"
	"testing"
)

// 本文件用**源码文本断言**锁死 reasoning replay 的接线位置。
//
// 为什么需要它: reasoning_replay_test.go 里的用例都是对**函数体**的复刻验证 ——
// 改坏 providers_chat.go 的接线不会让它们变红, 因为复刻体照旧正确。这类测试只能
// 证明"逻辑对", 证明不了"接线在、且接在对的作用域里"。
//
// 而本项恰恰是"作用域抄错比不抄更坏"的典型: 参考实现的 gate 是
// `targetFormat === FORMATS.OPENAI && !requiresExplicitReasoningReplay`,
// 若把调用挪进 anthropic 分支(或改成无条件跑), 复刻测试全绿但线上行为错了。
// 所以这里做**文本级**断言, 故意笨拙而精确。

// TestReplay接线_源码层锁死 检查真实调用点的三个必要属性。
func TestReplay接线_源码层锁死(t *testing.T) {
	src, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读 providers_chat.go 失败: %v", err)
	}
	s := string(src)

	// ① 调用必须存在, 且用的是包装函数(不是裸的 inject*, 那样会丢掉 explicit gate)。
	const call = "applyEmptyReasoningReplay(msgs, normalizeProviderID(p.name), model, params)"
	if !strings.Contains(s, call) {
		t.Fatalf("接线缺失: %q 未在 providers_chat.go 中被调用", call)
	}
	// 裸调用 injectEmptyReasoningContentForToolCalls 会绕过 explicit gate —— 禁止。
	if strings.Contains(s, "= injectEmptyReasoningContentForToolCalls(") {
		t.Fatal("禁止在 chatWithKey 里裸调 injectEmptyReasoningContentForToolCalls" +
			"（会绕过 requiresExplicitReasoningReplay 这道 gate）")
	}

	// ② 必须在 anthropic 分支**之外** —— 即挂在 `} else if msgs, ok := params["messages"]; ok {` 上。
	//
	//    参考实现是 `targetFormat === FORMATS.OPENAI`; 我方用 "非 anthropic" 表达。
	//    若有人把调用挪进 anthropic 分支, 则 `} else if` 这行会消失、call 会出现在
	//    anthropic 分支的闭合括号之前 —— 下面两条断言能同时抓住。
	const elseIf = `} else if msgs, ok := params["messages"]; ok {`
	iElse := strings.Index(s, elseIf)
	if iElse < 0 {
		t.Fatalf("接线作用域错误: 未找到 %q —— 说明 replay 调用没有挂在 anthropic 分支的 else 上", elseIf)
	}
	iCall := strings.Index(s, call)
	if iCall < 0 {
		t.Fatal("接线缺失: 调用片段不存在")
	}
	if iCall < iElse {
		t.Fatal("接线作用域错误: replay 调用出现在 `} else if` 之前（即落进了 anthropic 分支）")
	}

	// 锚定 anthropic 分支的起点, 用它界定"else 分支必须在它之后"。
	const anthropicIf = `if cfg.APIType == "anthropic" {`
	iAnthropic := strings.Index(s, anthropicIf)
	if iAnthropic < 0 {
		t.Fatalf("未找到 %q", anthropicIf)
	}
	if iElse < iAnthropic {
		t.Fatal("`} else if` 出现在 anthropic 分支之前 —— 结构不对")
	}

	// ③ 必须传 `p.name`(真实 provider 名) 而不是空串或归一后的别名。
	//
	//    判定表里既有 provider 白名单也有 model 正则, 两者都依赖"真实上游值"。
	//    参考实现同样传 `provider` 与 `model`(translator/index.ts:618)。
	if !strings.Contains(call, "normalizeProviderID(p.name)") {
		t.Fatal("接线错误: provider 实参必须是 normalizeProviderID(p.name)")
	}
	if !strings.Contains(call, ", model, params)") {
		t.Fatal("接线错误: model 实参必须是 chatWithKey 开头取到的原始 params[\"model\"]")
	}

	// ④ 包装函数内部必须用 allowLegacyFallback=false + hasThinkingConfig ——
	//    这是与 inject 内部判定**故意不对称**的那一半, 抄错就是另一套语义。
	wrap, err := os.ReadFile("reasoning_replay.go")
	if err != nil {
		t.Fatalf("读 reasoning_replay.go 失败: %v", err)
	}
	w := string(wrap)
	const gate = `requiresReasoningReplay(provider, model, "", hasThinkingConfig(params), false)`
	if !strings.Contains(w, gate) {
		t.Fatalf("接线 gate 抄错: 期望 %q", gate)
	}
	// 反向: 不得退化成 allowLegacyFallback=true（那会让白名单 provider 全都被跳过）。
	if strings.Contains(w, `hasThinkingConfig(params), true)`) {
		t.Fatal("接线 gate 抄错: allowLegacyFallback 必须为 false（参考实现是 requiresExplicitReasoningReplay）")
	}
}

// TestReplay接线_else分支未吞掉anthropic守卫 防止"把 else 写成平行 if"。
//
// 若有人写成 `if cfg.APIType != "anthropic" { ... }` 与 anthropic 分支并列,
// 语义上仍可接受(互斥), 但那会让"anthropic 分支内 messages 已被改写"这一前提
// 变得不显眼。此处不强制写法, 只断言**不存在**把 replay 挂在 anthropic 的
// 第一条守卫之上的情形 —— 即 anthropicIf 必须早于 elseIf。
func TestReplay接线_else分支未吞掉anthropic守卫(t *testing.T) {
	src, err := os.ReadFile("providers_chat.go")
	if err != nil {
		t.Fatalf("读 providers_chat.go 失败: %v", err)
	}
	s := string(src)
	iA := strings.Index(s, `if cfg.APIType == "anthropic" {`)
	iE := strings.Index(s, `} else if msgs, ok := params["messages"]; ok {`)
	if iA < 0 || iE < 0 {
		t.Fatalf("结构缺失: anthropicIf=%d elseIf=%d", iA, iE)
	}
	if iA > iE {
		t.Fatal("anthropic 守卫必须早于 else 分支")
	}
}
