package stealth

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestScrubRequestBodyDeletesClientFieldsInPlace(t *testing.T) {
	body := []byte(`{"model":"x","metadata":{"user_id":"u","session_id":"s","client_id":"c","device_id":"d","keep":"ok"},"x-request-id":"rid","messages":[{"role":"user","content":"hello"}]}`)
	first := &body[0]

	out := ScrubRequestBody(body)
	if len(out) == 0 {
		t.Fatal("scrubbed body should not be empty")
	}
	if &out[0] != first {
		t.Fatal("scrub should delete fields in place to avoid a second long-context body copy")
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("scrubbed body must remain valid JSON: %v\n%s", err, out)
	}
	if _, ok := got["x-request-id"]; ok {
		t.Fatal("x-request-id should be scrubbed")
	}
	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata should remain an object")
	}
	for _, key := range []string{"user_id", "session_id", "client_id", "device_id"} {
		if _, ok := metadata[key]; ok {
			t.Fatalf("metadata.%s should be scrubbed", key)
		}
	}
	if metadata["keep"] != "ok" {
		t.Fatalf("non-identifying metadata changed: %#v", metadata["keep"])
	}
	if got["model"] != "x" {
		t.Fatalf("model changed: %#v", got["model"])
	}
}

func TestScrubRequestBodyHandlesFieldPositions(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"x-request-id":"rid","model":"x"}`),
		[]byte(`{"model":"x","x-request-id":"rid"}`),
		[]byte(`{"metadata":{"user_id":"u"},"model":"x"}`),
		[]byte(`{"model":"x","metadata":{"user_id":"u"}}`),
	}
	for _, tc := range cases {
		out := ScrubRequestBody(append([]byte(nil), tc...))
		if !json.Valid(out) {
			t.Fatalf("scrubbed body must remain valid JSON: %s -> %s", tc, out)
		}
		if bytes.Contains(out, []byte("rid")) || bytes.Contains(out, []byte("user_id")) {
			t.Fatalf("client field was not scrubbed: %s -> %s", tc, out)
		}
	}
}

func TestScrubRequestBodyLeavesUnrelatedBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`)
	out := ScrubRequestBody(body)
	if !bytes.Equal(out, body) {
		t.Fatalf("unrelated body changed: %s", out)
	}
}
