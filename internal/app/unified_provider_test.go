package app

import "testing"

func TestNormalizeModelID(t *testing.T) {
	p, m := normalizeModelID("zen/mimo-v2.5-free")
	if p != "opencode" || m != "mimo-v2.5-free" {
		t.Fatalf("got %s/%s", p, m)
	}
	p, m = normalizeModelID("openrouter:z-ai/glm-5.3:free")
	if p != "openrouter" || m != "z-ai/glm-5.3:free" {
		t.Fatalf("got %s/%s", p, m)
	}
	p, m = normalizeModelID("cline-pass/deepseek-v4-flash")
	if p != "clinepass" || m != "deepseek-v4-flash" {
		t.Fatalf("got %s/%s", p, m)
	}
}
