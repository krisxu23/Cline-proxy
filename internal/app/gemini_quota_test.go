package app

import "testing"

func quotaPayload(message string, limit int, quotaID string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"details": []any{
				map[string]any{
					"@type": "type.googleapis.com/google.rpc.QuotaFailure",
					"violations": []any{
						map[string]any{
							"quotaMetric": "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
							"quotaId":     quotaID,
						},
					},
				},
			},
		},
	}
}

func TestParseQuotaFailureDailyExhausted(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerDayPerProjectPerModel-FreeTier, limit: 20, model: gemini-3.8-flash",
		20, "GenerateRequestsPerDayPerProjectPerModel-FreeTier")
	qf := parseQuotaFailure(payload)
	if qf == nil {
		t.Fatal("quota failure must parse")
	}
	if qf.NoFreeTier {
		t.Fatal("free tier exists")
	}
	if qf.ExhaustedWindow != "day" {
		t.Fatalf("window: %q", qf.ExhaustedWindow)
	}
	if qf.DailyRequestLimit == nil || *qf.DailyRequestLimit != 20 {
		t.Fatalf("daily limit: %+v", qf.DailyRequestLimit)
	}
}

func TestParseQuotaFailureNoFreeTier(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerDayPerProjectPerModel-FreeTier, limit: 0",
		0, "GenerateRequestsPerDayPerProjectPerModel-FreeTier")
	qf := parseQuotaFailure(payload)
	if qf == nil || !qf.NoFreeTier {
		t.Fatalf("limit 0 must mean no free tier: %+v", qf)
	}
}

func TestParseQuotaFailureRetryDelay(t *testing.T) {
	payload := quotaPayload(
		"Quota exceeded for metric: GenerateRequestsPerMinutePerProjectPerModel-FreeTier, limit: 5",
		5, "GenerateRequestsPerMinutePerProjectPerModel-FreeTier")
	payload["error"].(map[string]any)["details"] = append(
		payload["error"].(map[string]any)["details"].([]any),
		map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "33s"},
	)
	qf := parseQuotaFailure(payload)
	if qf == nil {
		t.Fatal("quota failure must parse")
	}
	if qf.RetryDelayMs != 33000 {
		t.Fatalf("retry delay: %d", qf.RetryDelayMs)
	}
	if qf.ExhaustedWindow != "minute" {
		t.Fatalf("minute window: %q", qf.ExhaustedWindow)
	}
}

func TestParseQuotaFailureNonQuota(t *testing.T) {
	if qf := parseQuotaFailure(map[string]any{"error": map[string]any{"message": "boom"}}); qf != nil {
		t.Fatal("non-quota payload must be nil")
	}
	if qf := parseQuotaFailure(nil); qf != nil {
		t.Fatal("nil payload must be nil")
	}
	if qf := parseQuotaFailure(map[string]any{}); qf != nil {
		t.Fatal("empty payload must be nil")
	}
}

func TestParseQuotaFailureRealGoogleMetric(t *testing.T) {
	// 真实 Google 429 的形态: message 与 quotaMetric 都是 snake_case 的 dotted
	// 指标, quotaId 才是 CamelCase 的窗口标识。两者都必须被解析出来,
	// 否则每日限额提取会静默失效。
	payload := map[string]any{
		"error": map[string]any{
			"message": "Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20, model: gemini-3.8-flash",
			"details": []any{
				map[string]any{
					"@type": "type.googleapis.com/google.rpc.QuotaFailure",
					"violations": []any{
						map[string]any{
							"quotaMetric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
							"quotaId":     "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
						},
					},
				},
			},
		},
	}
	qf := parseQuotaFailure(payload)
	if qf == nil {
		t.Fatal("real google payload must parse")
	}
	if qf.NoFreeTier {
		t.Fatal("free tier exists")
	}
	if qf.ExhaustedWindow != "day" {
		t.Fatalf("window: %q", qf.ExhaustedWindow)
	}
	if qf.DailyRequestLimit == nil || *qf.DailyRequestLimit != 20 {
		t.Fatalf("daily limit: %+v", qf.DailyRequestLimit)
	}
}

func TestPermanentRejectionReason(t *testing.T) {
	withdrawn := map[string]any{"error": map[string]any{"message": "model x is no longer available"}}
	if got := permanentRejectionReason(404, withdrawn); got != "withdrawn upstream" {
		t.Fatalf("404 withdrawn: %q", got)
	}
	if got := permanentRejectionReason(404, nil); got == "" {
		t.Fatal("bare 404 must still be permanent")
	}
	interactions := map[string]any{"error": map[string]any{"message": "model x only supports the Interactions API"}}
	if got := permanentRejectionReason(400, interactions); got != "not a chat-completions model" {
		t.Fatalf("400 interactions: %q", got)
	}
	if got := permanentRejectionReason(500, map[string]any{}); got != "" {
		t.Fatalf("5xx must not be permanent: %q", got)
	}
	if got := permanentRejectionReason(429, map[string]any{}); got != "" {
		t.Fatalf("429 must not be permanent: %q", got)
	}
}
