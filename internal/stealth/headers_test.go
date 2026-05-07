package stealth

import (
	"net/http"
	"testing"
)

func TestScrubProxyRemovesHopByHopConnectionTokens(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer keep")
	h.Set("Connection", "X-Transient, Upgrade, keep-alive")
	h.Set("X-Transient", "remove")
	h.Set("Upgrade", "websocket")
	h.Set("Keep-Alive", "timeout=5")

	ScrubProxy(h)

	for _, key := range []string{"Connection", "X-Transient", "Upgrade", "Keep-Alive"} {
		if got := h.Get(key); got != "" {
			t.Fatalf("%s should be removed, got %q", key, got)
		}
	}
	if got := h.Get("Authorization"); got != "Bearer keep" {
		t.Fatalf("authorization should be preserved, got %q", got)
	}
}

func TestScrubProxyRemovesProxyAndTracingDisclosureHeaders(t *testing.T) {
	h := http.Header{}
	for _, key := range []string{
		"X-Forwarded-For",
		"X-Forwarded-Uri",
		"X-Forwarded-Scheme",
		"X-Real-IP",
		"Forwarded",
		"Via",
		"CF-Connecting-IP",
		"X-Envoy-External-Address",
		"X-Envoy-Internal",
		"X-Cache",
		"X-Served-By",
		"X-Litellm-Model-Id",
		"Helicone-Request-Id",
		"X-Portkey-Trace-Id",
		"CF-AIG-Request-Id",
		"X-Kong-Proxy-Latency",
		"X-BT-Request-Id",
		"X-Amzn-Trace-Id",
		"Traceparent",
		"Tracestate",
		"Baggage",
		"X-Client-Version",
		"Sec-CH-UA",
	} {
		h.Set(key, "remove")
	}
	h.Set("User-Agent", "keep")
	h.Set("Accept", "application/json")

	ScrubProxy(h)

	for key := range h {
		switch key {
		case "User-Agent", "Accept":
			continue
		default:
			t.Fatalf("unexpected header survived: %s=%q", key, h.Get(key))
		}
	}
}
