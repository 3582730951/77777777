package server

import "testing"

func TestDecodeResponsesAPICompactPreservesServiceTier(t *testing.T) {
	req, err := decodeResponsesAPICompact([]byte(`{
		"model": "gpt-5.5",
		"instructions": "compact",
		"service_tier": "priority",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("decode compact: %v", err)
	}
	if req.ServiceTier != "priority" {
		t.Fatalf("service tier = %q, want priority", req.ServiceTier)
	}
}

func TestDecodeResponsesAPICompactPreservesDefaultServiceTierSentinel(t *testing.T) {
	req, err := decodeResponsesAPICompact([]byte(`{
		"model": "gpt-5.5",
		"service_tier": "default",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("decode compact: %v", err)
	}
	if req.ServiceTier != "default" {
		t.Fatalf("service tier = %q, want default", req.ServiceTier)
	}
}
