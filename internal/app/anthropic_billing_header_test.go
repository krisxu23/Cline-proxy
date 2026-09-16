package app

import "testing"

// 本文件锁定 anthropic_billing_header.go 的权威值。
//
// ★ 期望值全部来自 Node 实跑参考实现（见 .negbak/probe_billingheader.mjs 的
// B1–B12 各栏），不是读正则推断的。

func TestStripAnthropicBillingHeader_标准形态(t *testing.T) {
	// 探针 B1/B2/B3/B7。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"标准头部+换行", "x-anthropic-billing-header: abc123\nHello", "Hello"},
		{"头部在末尾(无换行)", "x-anthropic-billing-header: abc123", ""},
		{"CRLF", "x-anthropic-billing-header: abc123\r\nHello", "Hello"},
		{"头部后只有换行", "x-anthropic-billing-header: abc\n", ""},
	}
	for _, tc := range cases {
		if got := stripAnthropicBillingHeader(tc.in); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStripAnthropicBillingHeader_大小写不敏感(t *testing.T) {
	// 探针 B4: `/i`。
	for _, in := range []string{
		"X-ANTHROPIC-BILLING-HEADER: ABC\nHello",
		"X-Anthropic-Billing-Header: ABC\nHello",
		"x-AnThRoPiC-bIlLiNg-HeAdEr: ABC\nHello",
	} {
		if got := stripAnthropicBillingHeader(in); got != "Hello" {
			t.Fatalf("%q: got %q, want %q", in, got, "Hello")
		}
	}
}

func TestStripAnthropicBillingHeader_只删第一行一次(t *testing.T) {
	// 探针 B5/B6: 无 `m` flag + 无 `g` flag。
	// B5: 中间行不删
	in5 := "Hello\nx-anthropic-billing-header: abc\nWorld"
	if got := stripAnthropicBillingHeader(in5); got != in5 {
		t.Fatalf("中间行应原样保留: got %q, want %q", got, in5)
	}
	// B6: 连续两行头部只删第一行
	if got := stripAnthropicBillingHeader("x-anthropic-billing-header: a\nx-anthropic-billing-header: b\nHello"); got != "x-anthropic-billing-header: b\nHello" {
		t.Fatalf("只应删第一行: got %q", got)
	}
}

func TestStripAnthropicBillingHeader_冒号后无空格(t *testing.T) {
	// 探针 B8。
	for _, in := range []string{
		"x-anthropic-billing-header:\nHello",
		"x-anthropic-billing-header:  \nHello",
	} {
		if got := stripAnthropicBillingHeader(in); got != "Hello" {
			t.Fatalf("%q: got %q, want %q", in, got, "Hello")
		}
	}
}

func TestStripAnthropicBillingHeader_不匹配原样(t *testing.T) {
	// 探针 B9: `^` 锚定字符串开头。
	for _, in := range []string{
		"Hello world",
		" x-anthropic-billing-header: abc\nHello",
		"prefix x-anthropic-billing-header: abc\nHello",
	} {
		if got := stripAnthropicBillingHeader(in); got != in {
			t.Fatalf("%q 应原样保留: got %q", in, got)
		}
	}
	// 探针 B11: 空串 -> 空串
	if got := stripAnthropicBillingHeader(""); got != "" {
		t.Fatalf("空串应返回空串, got %q", got)
	}
}

func TestStripAnthropicBillingHeader_单独CR被当作行内容(t *testing.T) {
	// 探针 B12: "x-anthropic-billing-header: a\rb\nHello" -> "Hello"
	//
	// `[^\n]*` 会把 `\r` 也吃掉(它不是 `\n`), 然后 `(?:\r?\n)?` 匹配 `\n`。
	// 于是整行(含那个孤立的 `\r`)都被删除。
	in := "x-anthropic-billing-header: a\rb\nHello"
	if got := stripAnthropicBillingHeader(in); got != "Hello" {
		t.Fatalf("孤立 \\r 应被当作行内容删除: got %q, want %q", got, "Hello")
	}
}

func TestStripAnthropicBillingHeader_非字符串返回空串(t *testing.T) {
	// ★ 探针 B10 —— 最容易抄错的一栏:
	//   非字符串**一律返回空串**, 不是"原样返回", 也不是 nil。
	for _, v := range []any{nil, 42, true, map[string]any{}, []any{}, []any{"a"}, 3.14} {
		if got := stripAnthropicBillingHeader(v); got != "" {
			t.Fatalf("%v (%T) 应返回空串, got %q", v, v, got)
		}
	}
}

func TestStripAnthropicBillingHeader_幂等(t *testing.T) {
	// 剥过之后不应再有可剥内容。
	once := stripAnthropicBillingHeader("x-anthropic-billing-header: abc\nHello")
	if twice := stripAnthropicBillingHeader(once); twice != once {
		t.Fatalf("应幂等: got %q, want %q", twice, once)
	}
}
