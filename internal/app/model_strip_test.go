package app

import (
	"reflect"
	"testing"
)

func TestShouldStripPart(t *testing.T) {
	cases := []struct {
		name     string
		part     any
		stripSet map[string]bool
		want     bool
	}{
		// 非 map → false
		{"non-map", "hello", map[string]bool{"image": true}, false},
		{"nil", nil, map[string]bool{"image": true}, false},

		// type 缺失或空 → false（modelStrip.ts:11）
		{"no type", map[string]any{"foo": "bar"}, map[string]bool{"image": true}, false},
		{"empty type", map[string]any{"type": ""}, map[string]bool{"image": true}, false},

		// 精确匹配 stripSet 里的 type
		{"exact image_url", map[string]any{"type": "image_url"}, map[string]bool{"image_url": true}, true},

		// "image" 别名匹配 image_url 和 image（modelStrip.ts:14）
		{"image alias → image_url", map[string]any{"type": "image_url"}, map[string]bool{"image": true}, true},
		{"image alias → image", map[string]any{"type": "image"}, map[string]bool{"image": true}, true},

		// "audio" 别名匹配 input_audio 和 audio（modelStrip.ts:15）
		{"audio alias → input_audio", map[string]any{"type": "input_audio"}, map[string]bool{"audio": true}, true},
		{"audio alias → audio", map[string]any{"type": "audio"}, map[string]bool{"audio": true}, true},

		// 不在 stripSet 里 → false
		{"text not stripped", map[string]any{"type": "text"}, map[string]bool{"image": true}, false},
		{"no strip set", map[string]any{"type": "image_url"}, map[string]bool{}, false},

		// 多个 strip type
		{"multi strip image_url", map[string]any{"type": "image_url"}, map[string]bool{"image": true, "audio": true}, true},
		{"multi strip text", map[string]any{"type": "text"}, map[string]bool{"image": true, "audio": true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldStripPart(tc.part, tc.stripSet)
			if got != tc.want {
				t.Fatalf("shouldStripPart(%#v, %#v) = %v, want %v", tc.part, tc.stripSet, got, tc.want)
			}
		})
	}
}

func TestStripIncompatibleMessageContent(t *testing.T) {
	// 1) 无消息 → 原样返回
	msgs, n := stripIncompatibleMessageContent(nil, []string{"image"})
	if len(msgs) != 0 || n != 0 {
		t.Fatalf("nil messages: got (%d msgs, %d removed), want (0, 0)", len(msgs), n)
	}

	// 2) 空 stripTypes → 原样返回
	in := []any{map[string]any{"role": "user", "content": "hi"}}
	msgs, n = stripIncompatibleMessageContent(in, nil)
	if len(msgs) != 1 || n != 0 || !reflect.DeepEqual(msgs, in) {
		t.Fatalf("empty stripTypes: got (%d msgs, %d removed), want (1, 0) unchanged", len(msgs), n)
	}

	// 3) 字符串 content（非数组）→ 原样返回
	in = []any{map[string]any{"role": "user", "content": "plain text"}}
	msgs, n = stripIncompatibleMessageContent(in, []string{"image"})
	if len(msgs) != 1 || n != 0 {
		t.Fatalf("string content: got (%d, %d), want (1, 0)", len(msgs), n)
	}

	// 4) 剥掉一个 image_url part，保留 text part
	in = []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image_url", "image_url": "http://x"},
			map[string]any{"type": "text", "text": "world"},
		},
	}}
	msgs, n = stripIncompatibleMessageContent(in, []string{"image"})
	if n != 1 {
		t.Fatalf("removedParts = %d, want 1", n)
	}
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2", len(content))
	}
	if content[0].(map[string]any)["text"] != "hello" || content[1].(map[string]any)["text"] != "world" {
		t.Fatalf("content = %+v, want [hello, world]", content)
	}

	// 5) 全部剥光 → 补占位文本（modelStrip.ts:48-51）
	in = []any{map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "image_url", "image_url": "http://x"}},
	}}
	msgs, n = stripIncompatibleMessageContent(in, []string{"image"})
	if n != 1 {
		t.Fatalf("removedParts = %d, want 1", n)
	}
	content = msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1 (placeholder)", len(content))
	}
	if content[0].(map[string]any)["text"] != "[unsupported image/audio content removed]" {
		t.Fatalf("placeholder text = %v, want '[unsupported image/audio content removed]'",
			content[0].(map[string]any)["text"])
	}

	// 6) audio 剥离
	in = []any{map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "input_audio", "input_audio": "base64..."},
			map[string]any{"type": "text", "text": "said hello"},
		},
	}}
	msgs, n = stripIncompatibleMessageContent(in, []string{"audio"})
	if n != 1 {
		t.Fatalf("audio removedParts = %d, want 1", n)
	}
	content = msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content = %+v, want [text]", content)
	}

	// 7) 浅拷贝纪律：剥完的 message 是新 map，不是入参本身
	in = []any{map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "image_url", "image_url": "x"}},
	}}
	msgs, _ = stripIncompatibleMessageContent(in, []string{"image"})
	original := in[0].(map[string]any)
	// 改原始 message 的 content 键，输出不受影响
	original["content"] = "MUTATED"
	outMsg := msgs[0].(map[string]any)
	if outMsg["content"] == "MUTATED" {
		t.Fatal("输出 message 与入参共享 content 引用，违反浅拷贝纪律")
	}

	// 8) 混合内容类型
	in = []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image_url", "image_url": "x"},
				map[string]any{"type": "input_audio", "input_audio": "y"},
				map[string]any{"type": "text", "text": "bye"},
			},
		},
		map[string]any{"role": "assistant", "content": "plain"}, // 字符串 content → 不动
	}
	msgs, n = stripIncompatibleMessageContent(in, []string{"image", "audio"})
	if n != 2 {
		t.Fatalf("removedParts = %d, want 2", n)
	}
	content = msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2", len(content))
	}
	if msgs[1] == nil {
		t.Fatal("字符串 content 的 message 不应该被丢弃")
	}
	if c, ok := msgs[1].(map[string]any)["content"].(string); !ok || c != "plain" {
		t.Fatalf("字符串 content 的 message 应该原样保留 content=\"plain\"，得到 %v", msgs[1].(map[string]any)["content"])
	}
}
