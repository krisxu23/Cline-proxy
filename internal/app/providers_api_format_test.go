package app

// 供应商协议形态(APIFormat)的测试。
//
// 三种形态与 opencode 官方端点矩阵一致(见 docs/opencode-zen-facts.md 第 1 节)。
// 关键约束: **APIFormat 为空时行为必须与改动前完全一致** —— 既有配置不能受影响。

import (
	"net/http"
	"strings"
	"testing"
)

func TestProviderAPIFormatPath(t *testing.T) {
	cases := []struct {
		format string
		want   string
	}{
		{"", "/chat/completions"},            // 未设置 = 默认, 必须与改动前一致
		{apiFormatChat, "/chat/completions"}, // 显式 chat
		{apiFormatMessages, "/messages"},
		{apiFormatResponses, "/responses"},
		{"unknown-value", "/chat/completions"}, // 未知值退化为默认, 不 panic
	}
	for _, c := range cases {
		cfg := providerConfig{BaseURL: "https://opencode.ai/zen/v1", APIFormat: c.format}
		got := cfg.chatEndpoint()
		want := "https://opencode.ai/zen/v1" + c.want
		if got != want {
			t.Fatalf("APIFormat=%q 的端点 = %q, 期望 %q", c.format, got, want)
		}
	}
}

// ★ 锁住那个真实的坑: base 已含 /v1, 相对路径必须是 /messages 而不是 /v1/messages。
func TestProviderAPIFormatPath_不重复版本段(t *testing.T) {
	cfg := providerConfig{BaseURL: "https://opencode.ai/zen/v1", APIFormat: apiFormatMessages}
	if got := cfg.chatEndpoint(); strings.Contains(got, "/v1/v1/") {
		t.Fatalf("路径出现重复版本段: %q", got)
	}
}

// BaseURL 结尾的斜杠要被规整掉, 否则会拼出 //messages。
func TestProviderAPIFormatPath_尾部斜杠(t *testing.T) {
	cfg := providerConfig{BaseURL: "https://example.com/v1/", APIFormat: apiFormatResponses}
	if got := cfg.chatEndpoint(); got != "https://example.com/v1/responses" {
		t.Fatalf("尾部斜杠未规整: %q", got)
	}
}

// 鉴权方言: APIFormat=messages 或 APIType=anthropic 都要走 Anthropic 方言。
//
// ★ 保留 APIType=anthropic 的原语义("走 chat 路径但用 Anthropic 鉴权"),
// 否则会改坏既有配置。
func TestProviderUsesAnthropicAuth(t *testing.T) {
	cases := []struct {
		name   string
		format string
		apiTyp string
		want   bool
	}{
		{"默认(chat + 无 APIType)", "", "", false},
		{"显式 chat", apiFormatChat, "", false},
		{"messages 协议", apiFormatMessages, "", true},
		{"responses 协议", apiFormatResponses, "", false}, // Responses 用 Bearer
		{"历史 APIType=anthropic", "", "anthropic", true},
		{"历史 APIType=anthropic + chat", apiFormatChat, "anthropic", true},
		{"APIType=openai", "", "openai", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := providerConfig{APIFormat: c.format, APIType: c.apiTyp}
			if got := cfg.usesAnthropicAuth(); got != c.want {
				t.Fatalf("usesAnthropicAuth = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// chat 形态的转换必须是**恒等**且不改动入参 —— 下游还要用 params。
func TestConvertOutboundRequest_chat形态恒等(t *testing.T) {
	cfg := providerConfig{} // APIFormat 为空 = chat
	params := map[string]any{"model": "m", "messages": []any{}}
	out, err := cfg.convertOutboundRequest("m", params, false)
	if err != nil {
		t.Fatalf("chat 形态不该报错: %v", err)
	}
	if len(out) != len(params) {
		t.Fatalf("chat 形态应原样返回, 实得 %v", out)
	}
}

// chat 形态的响应转换必须是恒等的 —— 不能包一层转换把响应体吃掉。
func TestConvertProviderResponse_chat形态恒等(t *testing.T) {
	cfg := providerConfig{}
	resp := &http.Response{StatusCode: 200}
	out, err := cfg.convertProviderResponse(resp, "m", false)
	if err != nil {
		t.Fatalf("chat 形态不该报错: %v", err)
	}
	if out != resp {
		t.Fatal("chat 形态应原样返回同一个 *http.Response")
	}
}
