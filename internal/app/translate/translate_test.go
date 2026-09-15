package translate

import "testing"

// 本包历史上守的是"重复转换致 Responses input 被转成空数组"的线上事故
// (编译通过、测试全绿, 只有真机 400 才暴露)。R2 审计 F4 指出本包曾是
// [no test files] —— 与"防事故"定位不符, 本文件把不变量固化进测试。

func TestValidateOutboundResponsesRejectsChatLeftovers(t *testing.T) {
	// 事故形态复现: responses 体里残留 chat 形状字段(未转换干净/重复转换)。
	problems := ValidateOutbound(Responses, map[string]any{
		"model":      "gpt-4o",
		"input":      []any{map[string]any{"role": "user"}},
		"messages":   []any{},
		"max_tokens": float64(100),
	})
	if len(problems) < 2 {
		t.Fatalf("应报出残留字段问题, got %v", problems)
	}
}

func TestValidateOutboundResponsesEmptyInputBlocked(t *testing.T) {
	// "input 被转成空数组"正是当年事故的直接表征 —— 必须拦截。
	problems := ValidateOutbound(Responses, map[string]any{
		"model": "gpt-4o",
		"input": []any{},
	})
	found := false
	for _, p := range problems {
		if p != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("空 input(无 previous_response_id)必须被判违例")
	}
}

func TestValidateOutboundResponsesPRIDExemptsInput(t *testing.T) {
	problems := ValidateOutbound(Responses, map[string]any{
		"model":                "gpt-4o",
		"previous_response_id": "resp_1",
	})
	if len(problems) != 0 {
		t.Fatalf("带 previous_response_id 时空 input 应合法, got %v", problems)
	}
}

func TestValidateOutboundChatAndAnthropic(t *testing.T) {
	if p := ValidateOutbound(Chat, map[string]any{"model": "m", "messages": []any{1}}); len(p) != 0 {
		t.Fatalf("合法 chat 体不应报问题: %v", p)
	}
	if p := ValidateOutbound(Chat, map[string]any{"model": "m", "input": []any{1}}); len(p) == 0 {
		t.Fatal("chat 体残留 responses 的 input 必须被拦截")
	}
	if p := ValidateOutbound(Anthropic, map[string]any{"model": "m", "messages": []any{1}, "max_tokens": float64(0)}); len(p) == 0 {
		t.Fatal("anthropic 体 max_tokens<=0 必须被拦截")
	}
	if p := ValidateOutbound(Anthropic, map[string]any{"model": "m", "messages": []any{1}, "max_tokens": float64(64)}); len(p) != 0 {
		t.Fatalf("合法 anthropic 体不应报问题: %v", p)
	}
	if p := ValidateOutbound(Kind("bogus"), map[string]any{"model": "m"}); len(p) == 0 {
		t.Fatal("未知形状必须报违例")
	}
	if p := ValidateOutbound(Chat, nil); len(p) == 0 {
		t.Fatal("nil 出站体必须报违例")
	}
}

func TestRegisterAndTranslateRequestRoundtrip(t *testing.T) {
	RegisterRequest(Chat, Responses, func(b map[string]any) map[string]any {
		out := map[string]any{"model": b["model"]}
		if msgs, ok := b["messages"].([]any); ok {
			out["input"] = msgs
		}
		return out
	})
	if !HasRequest(Chat, Responses) {
		t.Fatal("注册后 HasRequest 应为真")
	}
	body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user"}}}
	got := TranslateRequest(Chat, Responses, body)
	if _, ok := got["input"].([]any); !ok {
		t.Fatalf("注册的转换未被应用: %v", got)
	}
	// 同形状 = 幂等原样返回(不重复转换 —— 事故的另一个可能诱因)。
	same := TranslateRequest(Chat, Chat, body)
	if same["messages"] == nil || same["input"] != nil {
		t.Fatal("同形状转换必须原样返回")
	}
	// 未注册方向: 原样返回(交给出站校验兜底), 不猜。
	unknown := TranslateRequest(Responses, Anthropic, body)
	if unknown["messages"] == nil {
		t.Fatal("未注册方向应原样返回")
	}
	// nil body 语义
	if TranslateRequest(Chat, Responses, nil) != nil {
		t.Fatal("nil body 应返回 nil")
	}
	// 诊断接口包含刚注册的方向
	found := false
	for _, d := range RegisteredDirections() {
		if d == "chat→responses" {
			found = true
		}
	}
	if !found {
		t.Fatalf("RegisteredDirections 缺少注册项: %v", RegisteredDirections())
	}
}
