package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmptyOpenAIChunk(t *testing.T) {
	ev := EmptyOpenAIChunk("m")
	choices, _ := ev.Payload["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices: %#v", ev.Payload)
	}
}

func TestHasFinishReason(t *testing.T) {
	if HasFinishReason(map[string]any{"choices": []any{map[string]any{"finish_reason": nil}}}) {
		t.Fatal("null finish_reason must not count")
	}
	if !HasFinishReason(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop"}}}) {
		t.Fatal("stop finish_reason must count")
	}
	if HasFinishReason(map[string]any{"no": "choices"}) {
		t.Fatal("missing choices must not count")
	}
}

func TestAppendStopChunkIfNoFinish(t *testing.T) {
	var buf bytes.Buffer
	if err := AppendStopChunkIfNoFinish(&buf, false, "m"); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("missing stop finish_reason: %s", s)
	}
	if strings.Contains(s, "[DONE]") {
		t.Fatalf("stop chunk helper must not write DONE: %s", s)
	}
	var buf2 bytes.Buffer
	if err := AppendStopChunkIfNoFinish(&buf2, true, "m"); err != nil {
		t.Fatal(err)
	}
	if buf2.Len() != 0 {
		t.Fatalf("expected no write when finish seen, got %q", buf2.String())
	}
}
