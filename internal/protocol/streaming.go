package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SSEEvent is a single Server-Sent Event payload normalized for downstream
// OpenAI-compatible clients. Payload is the parsed JSON object when the
// upstream emits one; Raw is set when the event is [DONE] or otherwise not
// parseable.
type SSEEvent struct {
	Payload map[string]any
	Raw     string // populated for "[DONE]" or non-JSON events
	Done    bool   // true when the event is the [DONE] sentinel
}

// ScanSSE returns a channel of SSEEvent values read from r. The channel is
// closed when EOF is reached or the reader returns an error. Empty `data:`
// lines are skipped (consistent with the OpenAI streaming contract).
//
// Borrowed from defyma/cline-proxy (Node): SSE boundary detection and
// split(\n\n) framing.
func ScanSSE(r io.Reader) (<-chan SSEEvent, <-chan error) {
	out := make(chan SSEEvent, 16)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		reader := bufio.NewReaderSize(r, 64*1024)
		var buf strings.Builder
		flush := func() {
			line := buf.String()
			buf.Reset()
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				return
			}
			if !strings.HasPrefix(line, "data:") {
				return
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				return
			}
			if payload == "[DONE]" {
				out <- SSEEvent{Raw: payload, Done: true}
				return
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				out <- SSEEvent{Raw: payload}
				return
			}
			// Some Cline responses wrap in {data: {...}}; unwrap one level.
			if d, ok := obj["data"].(map[string]any); ok {
				obj = d
			}
			out <- SSEEvent{Payload: obj}
		}
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				buf.WriteString(line)
				if strings.HasSuffix(strings.TrimRight(line, "\r\n"), "\n") {
					// already terminated by newline inside line; flush below
				}
				// Flush on blank line (event boundary).
				if strings.TrimRight(line, "\r\n") == "" {
					flush()
				}
			}
			if err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "EOF") {
					flush()
					return
				}
				errc <- err
				return
			}
		}
	}()
	return out, errc
}

// EmptyOpenAIChunk is appended to a stream when the upstream closes
// without emitting any choices; this prevents OpenAI-compatible clients
// from surfacing "Provider returned no completion choices" errors.
//
// Borrowed from defyma/cline-proxy: its pipeSse() appends an empty chunk
// when sentChoice is false. Reproduced here with attribution.
func EmptyOpenAIChunk(model string) SSEEvent {
	if model == "" {
		model = "unknown"
	}
	return SSEEvent{
		Payload: map[string]any{
			"id":      fmt.Sprintf("chatcmpl_%d", NowMillis()),
			"object":  "chat.completion.chunk",
			"created": NowSecs(),
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":        0,
					"delta":        map[string]any{"content": ""},
					"finish_reason": "stop",
				},
			},
		},
	}
}

// SanitizeContent is currently a no-op, but kept as a hook so future
// transformations (e.g. stripping <thinking> blocks for non-supporting
// clients) can land in one place.
func SanitizeContent(s string) string {
	return s
}

// ReasonToAnthropicStop maps an OpenAI finish_reason to the equivalent
// Anthropic stop_reason string. Used by the Anthropic streaming emitter.
func ReasonToAnthropicStop(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// AppendStopChunkIfNoFinish writes a final chunk carrying
// finish_reason "stop" when the upstream stream ended without any
// finish_reason, so OpenAI-compatible clients (e.g. the Cline extension)
// do not fail with "Stream ended without finish_reason". No-op when a
// finish_reason was already seen.
func AppendStopChunkIfNoFinish(w io.Writer, sawFinish bool, model string) error {
	if sawFinish {
		return nil
	}
	b, err := json.Marshal(EmptyOpenAIChunk(model).Payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(b))
	return err
}

// AppendEmptyChunkIfNoChoice is a small convenience that the proxy layer
// can call after the upstream stream closes; if no SSEEvent reported a
// choices array, the helper writes an empty chunk to w (in OpenAI SSE
// format) so clients never see a "no completion choices" error.
func AppendEmptyChunkIfNoChoice(w io.Writer, emittedChoice bool, model string) error {
	if emittedChoice {
		return nil
	}
	ev := EmptyOpenAIChunk(model)
	b, err := json.Marshal(ev.Payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", string(b)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return nil
}
