package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2.1: direct RemoteAddr stays as-is even with spoofed headers.
func TestClientIPDirectRemoteAddrAsIs(t *testing.T) {
	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := traceFrom(r.Context())
		if tr == nil {
			t.Fatal("missing trace")
		}
		if got := tr.snapshot().RequestID; got == "" {
			t.Fatal("missing request id")
		}
		if tr.ClientIP != "203.0.113.9" {
			t.Fatalf("direct public peer must stay as-is, got %q", tr.ClientIP)
		}
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "10.9.9.9, 192.168.1.1")
	req.Header.Set("X-Real-IP", "10.9.9.8")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("X-Request-Id echo must be kept")
	}
}

// 2.1: trusted proxy (loopback/private peer) -> first public IP wins.
func TestClientIPTrustedProxyFirstPublicWins(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		want       string
	}{
		{"loopback peer with XFF chain", "127.0.0.1:5000", "203.0.113.7, 10.0.0.1, 192.168.1.1", "", "203.0.113.7"},
		{"private peer with XFF chain", "10.0.0.5:5000", "10.9.9.9, 198.51.100.23", "", "198.51.100.23"},
		{"loopback peer with X-Real-IP", "127.0.0.1:5000", "", "198.51.100.44", "198.51.100.44"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = traceFrom(r.Context()).ClientIP
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
			req.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if c.xri != "" {
				req.Header.Set("X-Real-IP", c.xri)
			}
			h.ServeHTTP(rec, req)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// 2.1: spoofed private never overrides.
func TestClientIPSpoofedPrivateNeverOverrides(t *testing.T) {
	// direct public peer + private-only XFF -> stays direct
	var got string
	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = traceFrom(r.Context()).ClientIP
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 192.168.1.2")
	h.ServeHTTP(rec, req)
	if got != "203.0.113.9" {
		t.Fatalf("spoofed private must never override direct, got %q", got)
	}
	// trusted peer but only private in headers -> fall back to peer
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req2.RemoteAddr = "127.0.0.1:5000"
	req2.Header.Set("X-Forwarded-For", "10.0.0.1, 192.168.1.2")
	h.ServeHTTP(rec2, req2)
	if got != "127.0.0.1" {
		t.Fatalf("private-only XFF must fall back to peer, got %q", got)
	}
}

func TestRouteChatPathsAlwaysClineWhenModelEmpty(t *testing.T) {
	for _, p := range []string{
		"/v1/chat/completions",
		"/chat/completions",
		"/v1/messages",
		"/messages",
		"/v1/responses",
		"/responses",
	} {
		t.Run(p, func(t *testing.T) {
			closeReqLogs()
			reqLogsMu.Lock()
			reqLogs = nil
			reqLogsMu.Unlock()
			defer closeReqLogs()
			h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			// empty body: model probe finds nothing
			req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
			h.ServeHTTP(rec, req)
			flushReqLogs()
			logs := LoadRequestLogs()
			if len(logs) != 1 {
				t.Fatalf("chat path %q with empty model must be logged (route=cline, not noise), got %d logs", p, len(logs))
			}
			if logs[0].Route != "cline" {
				t.Fatalf("path %q with empty model must route=cline, got %q", p, logs[0].Route)
			}
		})
	}
	// unchanged classifications: meta/health stay noise (not logged), admin GET not logged
	for _, p := range []string{"/v1/models", "/healthz", "/admin/models"} {
		closeReqLogs()
		reqLogsMu.Lock()
		reqLogs = nil
		reqLogsMu.Unlock()
		h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		h.ServeHTTP(rec, req)
		flushReqLogs()
		if n := len(LoadRequestLogs()); n != 0 {
			t.Fatalf("polling path %q must stay noise (0 logs), got %d", p, n)
		}
	}
	closeReqLogs()
}

// 2.4: successful chat never noise; only meta/health polling is noise.
func TestNoiseSuccessfulChatNeverNoise(t *testing.T) {
	// correctly routed chat with usage -> never noise
	if isRequestNoise(RequestLog{Route: "cline", Method: "POST", Status: 200, UsageReported: true}) {
		t.Fatal("successful chat with usage must never be noise")
	}
	if isRequestNoise(RequestLog{Route: "zen", Method: "POST", Status: 200, UsageReported: true}) {
		t.Fatal("successful zen chat with usage must never be noise")
	}
	// even misrouted-but-with-usage -> never noise (proves chat success)
	if isRequestNoise(RequestLog{Route: "other", Method: "POST", Status: 200, UsageReported: true}) {
		t.Fatal("2xx with usage must never be noise even if route=other")
	}
	if !isRequestNoise(RequestLog{Route: "meta", Method: "GET", Status: 200}) {
		t.Fatal("meta polling must stay noise")
	}
	if !isRequestNoise(RequestLog{Route: "other", Method: "GET", Status: 200}) {
		t.Fatal("other polling must stay noise")
	}
	if !isRequestNoise(RequestLog{Route: "admin", Method: "GET", Status: 200}) {
		t.Fatal("admin GET must stay noise")
	}
	// errors are never noise
	if isRequestNoise(RequestLog{Route: "other", Method: "GET", Status: 500}) {
		t.Fatal("5xx must never be noise")
	}
}
