package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"cline-go-proxy/internal/protocol"
	"cline-go-proxy/internal/providers"
)

// ClinePass handler paths: cline-pass/ prefixed models route through the
// ClinePass key pool. The upstream speaks OpenAI chat format, so the
// existing stream converters (Anthropic events, Responses events) are
// reused directly over the raw upstream response.

func clinePassProvider() *providers.ClinePassProvider {
	return getGateway().ClinePass
}

// handleClinePassChat serves POST /v1/chat/completions with cline-pass/ models.
func handleClinePassChat(w http.ResponseWriter, r *http.Request, params map[string]any, isStream bool) {
	model, _ := params["model"].(string)
	cp := clinePassProvider()
	req := paramsToChatRequest(params, model, isStream)
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		if err := cp.ChatStream(r.Context(), req, w, nil); err != nil {
			log.Printf("  clinepass stream error: %v", err)
			tracker.finish(false, http.StatusBadGateway)
			return
		}
		tracker.finish(true, http.StatusOK)
		return
	}

	resp, err := cp.Chat(r.Context(), req)
	if err != nil {
		log.Printf("  clinepass api error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	tracker.finish(true, http.StatusOK)
	writeJSON(w, http.StatusOK, resp.Body)
}

// handleClinePassAnthropic serves POST /v1/messages with cline-pass/ models.
func handleClinePassAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cp := clinePassProvider()
	isStream := req.Stream
	creq := paramsToChatRequest(openAIReq, req.Model, isStream)
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        req.Model,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	up, err := cp.RawChat(r.Context(), creq, isStream)
	if err != nil {
		log.Printf("  clinepass anthropic error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	defer up.Body.Close()
	tracker.rec.Status = up.StatusCode

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		handleAnthropicStreamWithUsage(w, up, req.Model, toolSchemas, nil)
		tracker.finish(true, up.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(up.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	if d, ok := raw["data"].(map[string]any); ok {
		raw = d
	}
	chatOut := protocol.NormalizeOpenAIChunk(raw)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, up.StatusCode)
}

// handleClinePassResponses serves POST /v1/responses with cline-pass/ models.
func handleClinePassResponses(w http.ResponseWriter, r *http.Request, params, chat map[string]any, isStream bool) {
	cp := clinePassProvider()
	model, _ := chat["model"].(string)
	creq := paramsToChatRequest(chat, model, isStream)
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           nowMillis(),
		Upstream:     upstreamClinePass,
		Model:        model,
		Stream:       isStream,
		PromptTokens: estimateJSON(chat),
	})

	up, err := cp.RawChat(r.Context(), creq, isStream)
	if err != nil {
		log.Printf("  clinepass responses error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	defer up.Body.Close()
	tracker.rec.Status = up.StatusCode

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		chatStreamToResponses(w, up, nil)
		tracker.finish(true, up.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(up.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	if d, ok := raw["data"].(map[string]any); ok {
		raw = d
	}
	writeJSON(w, http.StatusOK, chatToResponses(raw))
	tracker.finish(true, up.StatusCode)
}

// nowMillis local alias keeps handler bodies tidy.
func nowMillis() int64 { return protocol.NowMillis() }

// registerClinePassAdminRoutes wires key management into the admin API.
func registerClinePassAdminRoutes(mux *http.ServeMux) {
	cp := clinePassProvider()
	mux.HandleFunc("/admin/api/clinepass/keys", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
		case "POST":
			var body struct {
				Key string `json:"key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body must be {\"key\": \"...\"}"})
				return
			}
			cp.AddKey(body.Key)
			writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
		default:
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		}
	}))
	mux.HandleFunc("/admin/api/clinepass/keys/delete", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
			return
		}
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body must be {\"key\": \"<masked>\"}"})
			return
		}
		cp.RemoveKey(body.Key)
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": cp.KeyStatuses()}})
	}))
	mux.HandleFunc("/admin/api/clinepass/models", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": clinePassProvider().ListModels()}})
	}))
	_ = fmt.Sprint() // keep fmt import if handlers change
}
