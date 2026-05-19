package openai_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/protocol/openai"
)

func TestDecodeBasic(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a bot."},{"role":"user","content":"Hello"}],"stream":true}`
	req, err := openai.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model want gpt-4o got %s", req.Model)
	}
	if !req.Stream {
		t.Errorf("stream want true")
	}
	if !strings.Contains(req.System, "bot") {
		t.Errorf("system not extracted: %q", req.System)
	}
	if len(req.Messages) != 1 {
		t.Errorf("messages want 1 (user only), got %d", len(req.Messages))
	}
	if req.Messages[0].Role != ir.RoleUser {
		t.Errorf("role want user, got %s", req.Messages[0].Role)
	}
}

func TestDecodeMultimodalText(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`
	req, err := openai.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 {
		t.Fatalf("expected 1 message with 1 part, got %+v", req.Messages)
	}
	if req.Messages[0].Parts[0].Text != "hi there" {
		t.Errorf("got %q", req.Messages[0].Parts[0].Text)
	}
}

func TestDecodePreservesFastServiceTier(t *testing.T) {
	body := `{"model":"gpt-5.5","service_tier":"fast","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req, err := openai.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.ServiceTier != "fast" {
		t.Fatalf("service tier not preserved: got %q want fast", req.ServiceTier)
	}
}

func TestDecodeResponsesPreservesFastServiceTier(t *testing.T) {
	body := `{"model":"gpt-5.5","input":[],"service_tier":"fast","reasoning":{"effort":"high"},"stream":true}`
	req, err := openai.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	if req.ServiceTier != "fast" {
		t.Fatalf("responses service tier not preserved: got %q want fast", req.ServiceTier)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("responses reasoning effort changed: got %q want high", req.ReasoningEffort)
	}
}

func TestStreamEncodeRoundTrip(t *testing.T) {
	rw := httptest.NewRecorder()
	enc := openai.NewEncoder(rw, "gpt-5.3", true)
	enc.WriteHeaders()
	ch := make(chan ir.Event, 4)
	ch <- ir.Event{Kind: ir.EvTextDelta, Text: "Hello "}
	ch <- ir.Event{Kind: ir.EvTextDelta, Text: "world"}
	ch <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	close(ch)
	if err := enc.Stream(ch); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Errorf("no SSE markers: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("no DONE marker")
	}
	if !strings.Contains(body, `"model":"gpt-5.3"`) {
		t.Errorf("display model not preserved")
	}
}

func TestNonStreamingResponse(t *testing.T) {
	rw := httptest.NewRecorder()
	enc := openai.NewEncoder(rw, "gpt-4o", false)
	enc.WriteHeaders()
	ch := make(chan ir.Event, 4)
	ch <- ir.Event{Kind: ir.EvTextDelta, Text: "answer"}
	ch <- ir.Event{Kind: ir.EvUsage, InputTokens: 5, OutputTokens: 3}
	ch <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	close(ch)
	if err := enc.Stream(ch); err != nil {
		t.Fatalf("stream: %v", err)
	}
	var out map[string]any
	if err := json.NewDecoder(bytes.NewReader(rw.Body.Bytes())).Decode(&out); err != nil {
		t.Fatalf("decode resp: %v ; body=%s", err, rw.Body.String())
	}
	if out["object"] != "chat.completion" {
		t.Errorf("expected chat.completion, got %v", out["object"])
	}
}
