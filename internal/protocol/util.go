package protocol

import (
	"fmt"
	"strings"
)

// getNested walks a parsed JSON map/slice tree. Returns nil if any hop
// fails. Mirrors internal/app/proxy.go getNested so we don't need to
// import that file's internals from here.
func getNested(obj any, keys ...any) any {
	current := obj
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			m, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			current = m[k]
		case int:
			arr, ok := current.([]any)
			if !ok || k >= len(arr) {
				return nil
			}
			current = arr[k]
		default:
			return nil
		}
	}
	return current
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toHex is a tiny helper for the various *_id fields (msg_, toolu_, ...).
// We avoid importing encoding/hex for trivial lowercase hex so the path
// stays cheap at the per-event scale of streaming responses.
func toHex(n int64) string {
	return fmt.Sprintf("%x", n)
}

// TrimBraces strips unbalanced trailing punctuation from a tool-args
// payload that some upstreams emit mid-stream. Borrowed from the
// existing internal/app/proxy.go parseToolArgs(); promoted here so
// protocol consumers can reuse it without dragging app imports.
func TrimBraces(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "{") && !strings.HasSuffix(s, "}") {
		s += "}"
	} else if strings.HasPrefix(s, "[") && !strings.HasSuffix(s, "]") {
		s += "]"
	}
	if strings.HasSuffix(s, ",") {
		s = strings.TrimRight(s, ",") + "}"
	}
	return s
}
