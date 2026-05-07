package gemini

import (
	"net/http"
	"testing"
)

func TestSetGeminiCLIHeaders(t *testing.T) {
	h := http.Header{}
	setGeminiCLIHeaders(h, "tok")

	want := map[string]string{
		"Authorization":     "Bearer tok",
		"Content-Type":      "application/json",
		"User-Agent":        geminiCLIUserAgent,
		"Accept":            "application/json",
		"X-Goog-Api-Client": "google-genai-sdk/1.41.0 gl-node/v22.19.0",
	}
	for key, value := range want {
		if got := h.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
}
