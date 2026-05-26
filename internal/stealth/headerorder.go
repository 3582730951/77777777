package stealth

import "net/http"

// OrderedHeaders sets headers on an *http.Request in the exact order
// specified, mimicking Node.js/Electron header emission order.
// This is a best-effort defense: Go's net/http doesn't guarantee
// serialization order, but setting headers on a fresh map in order
// gives the best chance of matching real client behavior.
//
// Node.js 24.x / Claude CLI header order:
//
//	Host, Connection, Content-Length, Content-Type, Authorization,
//	anthropic-beta, anthropic-version, user-agent, x-stainless-*,
//	accept, accept-encoding
//
// Call this AFTER setting all headers, it re-inserts them in order.
func OrderHeaders(req *http.Request) {
	if req == nil || req.Header == nil {
		return
	}

	// Define the canonical order for each provider's headers.
	order := []string{
		"Host",
		"Connection",
		"Content-Length",
		"Content-Type",
		"Authorization",
		"Anthropic-Beta",
		"Anthropic-Version",
		"OpenAI-Beta",
		"ChatGPT-Account-Id",
		"X-OpenAI-Fedramp",
		"User-Agent",
		"X-Stainless-Lang",
		"X-Stainless-Package-Version",
		"X-Stainless-Os",
		"X-Stainless-Arch",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Retry-Count",
		"X-Stainless-Timeout",
		"X-App",
		"Accept",
		"Accept-Encoding",
	}

	// Snapshot current headers.
	snapshot := make(map[string][]string, len(req.Header))
	for k, v := range req.Header {
		snapshot[k] = v
	}

	// Clear and re-insert in order.
	for k := range req.Header {
		delete(req.Header, k)
	}

	// First, insert known headers in order.
	for _, k := range order {
		if v, ok := snapshot[http.CanonicalHeaderKey(k)]; ok {
			req.Header[http.CanonicalHeaderKey(k)] = v
			delete(snapshot, http.CanonicalHeaderKey(k))
		}
	}

	// Then, append any remaining headers.
	for k, v := range snapshot {
		req.Header[k] = v
	}
}
