package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// estimateText / estimateJSON
// ============================================================================

func TestEstimateText(t *testing.T) {
	if estimateText("") != 0 {
		t.Fatalf("empty string -> 0, got %d", estimateText(""))
	}
	if estimateText("abcd") != 1 {
		t.Fatalf("4 runes -> 1, got %d", estimateText("abcd"))
	}
	if estimateText(strings.Repeat("z", 40)) != 10 {
		t.Fatalf("40 runes -> 10, got %d", estimateText(strings.Repeat("z", 40)))
	}
}

func TestEstimateJSONMonotonic(t *testing.T) {
	// 更大输入必须得到更大的估值(压缩预算选择的依据, 必须单调)。
	small := map[string]any{"a": "x"}
	big := map[string]any{"a": strings.Repeat("y", 10000)}
	if estimateJSON(big) <= estimateJSON(small) {
		t.Fatalf("estimateJSON not monotonic: big=%d small=%d", estimateJSON(big), estimateJSON(small))
	}
}

func TestEstimateJSONUnmarshalable(t *testing.T) {
	// 无法 marshal 的值应安全返回 0 而非 panic。
	if v := estimateJSON(map[string]any{"ch": make(chan int)}); v != 0 {
		t.Fatalf("expected 0 for unmarshalable, got %d", v)
	}
}

// ============================================================================
// selectRecent
// ============================================================================

func TestSelectRecent(t *testing.T) {
	mk := func(runes int) string { return strings.Repeat("a", runes) } // estimateText = runes/4

	t.Run("all fits -> nil", func(t *testing.T) {
		// 每个约 est 10(token), 共 30, 预算 1000 放得下 -> nil
		serialized := []string{mk(40), mk(40), mk(40)}
		if sel := selectRecent(serialized, 1000); sel != nil {
			t.Fatalf("expected nil when all fits, got %#v", sel)
		}
	})

	t.Run("partial overflow splits", func(t *testing.T) {
		serialized := []string{mk(40), mk(40), mk(40)} // 每个 est 10, 共 30
		sel := selectRecent(serialized, 25)
		if sel == nil {
			t.Fatalf("expected non-nil split")
		}
		if len(sel.recent) == 0 {
			t.Fatalf("recent must not be empty")
		}
		// 尾部原始消息必须保留在 recent 里
		if sel.recent[len(sel.recent)-1] != serialized[2] {
			t.Fatalf("last original message must be in recent tail")
		}
		if len(sel.head) == 0 {
			t.Fatalf("head must not be empty")
		}
		if sel.split < 1 {
			t.Fatalf("split must be >=1, got %d", sel.split)
		}
	})

	t.Run("tiny budget keeps tail", func(t *testing.T) {
		serialized := []string{mk(400)} // est 100
		sel := selectRecent(serialized, 1)
		if sel == nil {
			t.Fatalf("expected non-nil")
		}
		if len(sel.recent) == 0 {
			t.Fatalf("recent must not be empty even with tiny budget")
		}
	})
}

// ============================================================================
// fallbackTruncate
// ============================================================================

func TestFallbackTruncate(t *testing.T) {
	t.Run("never empty", func(t *testing.T) {
		params := map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "sys"},
				map[string]any{"role": "user", "content": strings.Repeat("x", 400)},
			},
		}
		m := &ZenModel{ID: "m", Context: 100}
		out := fallbackTruncate(params, m)
		if !out.changed {
			t.Fatalf("expected changed=true")
		}
		msgs := params["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("fallbackTruncate must never produce empty messages")
		}
		first := asMap(t, msgs[0])
		if first["role"] != "system" {
			t.Fatalf("first msg should be system truncation notice, got %#v", first)
		}
		if !strings.Contains(fmt.Sprint(first["content"]), "context compaction") {
			t.Fatalf("truncation notice missing: %v", first["content"])
		}
	})

	t.Run("empty messages unchanged", func(t *testing.T) {
		params := map[string]any{"messages": []any{}}
		m := &ZenModel{ID: "m", Context: 1000}
		out := fallbackTruncate(params, m)
		if out.changed {
			t.Fatalf("empty messages should not be compacted")
		}
	})
}

// ============================================================================
// maybeCompact
// ============================================================================

func TestMaybeCompact(t *testing.T) {
	orig := zenConfig
	defer func() { zenConfig = orig }()
	zenConfig = &zenConfigData{
		Enabled:    true,
		Compaction: zenCompactConfig{Auto: true, Buffer: 10, KeepTokens: 8000, MaxSummary: 4096},
	}

	t.Run("under threshold unchanged", func(t *testing.T) {
		m := &ZenModel{ID: "big", Context: 1000000} // threshold = 1000000 - max(0,10) = 999990
		params := map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "short"}},
		}
		out := maybeCompact(context.Background(), params, m, "sess-under")
		if out.changed {
			t.Fatalf("should not compact under threshold, got note=%q", out.note)
		}
		if _, ok := params["messages"]; !ok {
			t.Fatalf("messages should be untouched under threshold")
		}
	})

	t.Run("over threshold compresses", func(t *testing.T) {
		m := &ZenModel{ID: "small", Context: 100} // threshold = 100 - max(0,10) = 90
		params := map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": strings.Repeat("x", 40000)},
			},
		}
		// 短超时: 无网络时 generateSummary 失败 -> fallbackTruncate(changed=true);
		// 即便真能联网也只会在 ctx 到期后回退, 整体仍返回 changed=true。
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out := maybeCompact(ctx, params, m, "sess-over")
		if !out.changed {
			t.Fatalf("should compact over threshold, got note=%q", out.note)
		}
	})
}

// ============================================================================
// findExistingSummary
// ============================================================================

func TestFindExistingSummary(t *testing.T) {
	t.Run("has summary", func(t *testing.T) {
		msgs := []any{
			map[string]any{"role": "system", "content": "[Conversation Summary]\n this is the summary"},
			map[string]any{"role": "user", "content": "hi"},
		}
		if s := findExistingSummary(msgs, 5); s != "this is the summary" {
			t.Fatalf("expected summary text, got %q", s)
		}
	})

	t.Run("previous summary variant", func(t *testing.T) {
		msgs := []any{
			map[string]any{"role": "system", "content": "[Previous Conversation Summary]\n older"},
		}
		if s := findExistingSummary(msgs, 1); s != "older" {
			t.Fatalf("expected older, got %q", s)
		}
	})

	t.Run("no summary", func(t *testing.T) {
		msgs := []any{map[string]any{"role": "user", "content": "hi"}}
		if s := findExistingSummary(msgs, 5); s != "" {
			t.Fatalf("expected empty, got %q", s)
		}
	})

	t.Run("upTo bounds", func(t *testing.T) {
		msgs := []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "[Conversation Summary]\ns2"},
		}
		if s := findExistingSummary(msgs, 1); s != "" {
			t.Fatalf("upTo=1 should not see summary, got %q", s)
		}
		if s := findExistingSummary(msgs, 2); s != "s2" {
			t.Fatalf("upTo=2 should find summary, got %q", s)
		}
	})
}
