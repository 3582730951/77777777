package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseHeaderHygieneRemovesDisclosureHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Connection", "X-Transient, keep-alive")
		w.Header().Set("X-Transient", "remove")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Via", "1.1 gateway")
		w.Header().Set("X-Request-Id", "rid")
		w.Header().Set("Traceparent", "00-abc-def-01")
		w.Header().Set("X-Powered-By", "framework")
		w.Header().Set("X-Forwarded-For", "10.0.0.1")
		w.Header().Set("X-Envoy-Upstream-Service-Time", "12")
		w.Header().Set("X-Kong-Proxy-Latency", "1")
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Served-By", "cache-iad")
		w.Header().Set("X-Litellm-Model-Id", "model")
		w.Header().Set("Helicone-Request-Id", "hel")
		w.Header().Set("X-Portkey-Trace-Id", "pk")
		w.Header().Set("CF-AIG-Request-Id", "cf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	responseHeaderHygieneMiddleware(next).ServeHTTP(rec, req)

	for _, key := range []string{
		"Via",
		"X-Request-Id",
		"Traceparent",
		"X-Powered-By",
		"X-Forwarded-For",
		"X-Envoy-Upstream-Service-Time",
		"X-Kong-Proxy-Latency",
		"X-Cache",
		"X-Served-By",
		"X-Litellm-Model-Id",
		"Helicone-Request-Id",
		"X-Portkey-Trace-Id",
		"CF-AIG-Request-Id",
		"Connection",
		"X-Transient",
		"Keep-Alive",
		"Upgrade",
	} {
		if got := rec.Header().Get(key); got != "" {
			t.Fatalf("%s should be removed, got %q", key, got)
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type should be preserved, got %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("streaming proxy buffering hint should be preserved, got %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body changed: %q", rec.Body.String())
	}
}

func TestResponseHeaderHygienePreservesWebSocketUpgradeHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("X-Request-Id", "rid")
		w.WriteHeader(http.StatusSwitchingProtocols)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	responseHeaderHygieneMiddleware(next).ServeHTTP(rec, req)

	if got := rec.Header().Get("Connection"); got != "Upgrade" {
		t.Fatalf("websocket connection header changed: %q", got)
	}
	if got := rec.Header().Get("Upgrade"); got != "websocket" {
		t.Fatalf("websocket upgrade header changed: %q", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("disclosure header should still be removed on upgrade: %q", got)
	}
}
