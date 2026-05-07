package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/stream"
	"github.com/tidwall/gjson"
)

func TestClassifyResponsesSwitchSignalUsageLimit(t *testing.T) {
	msg := "■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at May 9th, 2026 6:12 PM."
	got := classifyResponsesSwitchSignal(msg, "")
	if !got.Retryable {
		t.Fatal("expected usage limit to be retryable")
	}
	if got.Class != domain.ErrQuotaExhausted {
		t.Fatalf("expected quota exhausted, got %s", got.Class)
	}
}

func TestClassifyResponsesSwitchSignalCapacity(t *testing.T) {
	msg := "⚠ Selected model is at capacity. Please try a different model."
	got := classifyResponsesSwitchSignal(msg, "")
	if !got.Retryable {
		t.Fatal("expected model capacity to be retryable")
	}
	if got.Class != domain.ErrRateLimited {
		t.Fatalf("expected rate limited, got %s", got.Class)
	}
}

func TestExpandResponsesBodyMergesPreviousTranscript(t *testing.T) {
	prev := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	}
	body := []byte(`{"model":"gpt-5.2","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}],"stream":true}`)
	merged := mergeResponsesTranscript(prev, responsesInputItems(body))
	expanded := expandResponsesBody(body, merged)

	var got struct {
		PreviousResponseID string            `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(expanded, &got); err != nil {
		t.Fatalf("expanded body invalid json: %v", err)
	}
	if got.PreviousResponseID != "" {
		t.Fatal("previous_response_id should be removed when replaying transcript to another account")
	}
	if len(got.Input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(got.Input))
	}
	if string(got.Input[0]) != string(prev[0]) {
		t.Fatalf("previous transcript not preserved: %s", got.Input[0])
	}
}

func TestResponsesInspectorDetectsTextualQuotaBeforeCommit(t *testing.T) {
	var insp responsesSSEInspector
	chunk := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"You've hit your usage limit."}` + "\n\n")
	got := insp.Feed(chunk)
	if !got.Retryable || got.Class != domain.ErrQuotaExhausted {
		t.Fatalf("expected retryable quota signal, got retryable=%v class=%s", got.Retryable, got.Class)
	}
}

func TestResponsesInspectorDetectsSplitTextualQuotaWithoutFullOutputScan(t *testing.T) {
	var insp responsesSSEInspector
	first := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"You've hit your "}` + "\n\n")
	second := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"usage limit."}` + "\n\n")
	if got := insp.Feed(first); got.Retryable {
		t.Fatalf("first partial delta should not be retryable: %v", got)
	}
	got := insp.Feed(second)
	if !got.Retryable || got.Class != domain.ErrQuotaExhausted {
		t.Fatalf("expected split quota signal, got retryable=%v class=%s", got.Retryable, got.Class)
	}
}

func TestResponsesInspectorCapturesOutputItemsForReplay(t *testing.T) {
	var insp responsesSSEInspector
	chunk := []byte("event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}` + "\n\n")
	got := insp.Feed(chunk)
	if got.Retryable {
		t.Fatal("function_call output item should not be retryable")
	}
	if len(insp.AssistantItems) != 1 {
		t.Fatalf("expected captured output item, got %d", len(insp.AssistantItems))
	}
	if string(insp.AssistantItems[0]) == "" || !gjson.GetBytes(insp.AssistantItems[0], "type").Exists() {
		t.Fatalf("captured item is invalid: %s", insp.AssistantItems[0])
	}
}

func TestResponsesInspectorCapsAssistantTranscriptCapture(t *testing.T) {
	oldLimit := responsesTranscriptCaptureMaxBytes
	responsesTranscriptCaptureMaxBytes = 8
	defer func() { responsesTranscriptCaptureMaxBytes = oldLimit }()

	var insp responsesSSEInspector
	chunk := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"abcdefghijklmnop"}` + "\n\n")

	got := insp.Feed(chunk)
	if got.Retryable {
		t.Fatalf("plain text delta should not be retryable: %v", got)
	}
	if !insp.CaptureOverflow {
		t.Fatal("expected capture overflow")
	}
	if text := insp.AssistantText.String(); text != "abcdefgh" {
		t.Fatalf("captured text = %q, want capped prefix", text)
	}
}

func TestForwardResponsesSSEMarksTranscriptTooLargeWithoutTruncatingDownstream(t *testing.T) {
	oldLimit := responsesTranscriptCaptureMaxBytes
	responsesTranscriptCaptureMaxBytes = 8
	defer func() { responsesTranscriptCaptureMaxBytes = oldLimit }()

	sse := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"abcdefghijklmnop"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp-large","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	rec := httptest.NewRecorder()

	result := forwardResponsesSSEWithHeadBuffer(context.Background(), strings.NewReader(sse), rec, nil, stream.HeadBufferConfig{
		MaxBytes: 4096, MaxEvents: 8,
	})

	if result.Err != nil {
		t.Fatalf("forward failed: %v", result.Err)
	}
	if !result.TranscriptTooLarge {
		t.Fatal("expected transcript-too-large marker")
	}
	if result.ResponseID != "resp-large" {
		t.Fatalf("response id = %q", result.ResponseID)
	}
	if !strings.Contains(rec.Body.String(), "abcdefghijklmnop") {
		t.Fatalf("downstream SSE was truncated: %s", rec.Body.String())
	}
}

func TestAppendAssistantOutputDoesNotDuplicateCapturedMessage(t *testing.T) {
	message := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`)
	got := appendAssistantOutput(nil, "hello", []json.RawMessage{message})
	if len(got) != 1 {
		t.Fatalf("expected only captured message item, got %d", len(got))
	}
	if string(got[0]) != string(message) {
		t.Fatalf("unexpected transcript item: %s", got[0])
	}
}

func TestApplyGroupSystemPromptToResponsesBodyPreservesPromptTextAndInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","instructions":"codex-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"long-context"}]}],"stream":true}`)
	group := &domain.Group{SystemPrompt: "group-policy"}

	got := applyGroupSystemPromptToResponsesBody(body, group)

	if string(body) != `{"model":"gpt-5.2","instructions":"codex-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"long-context"}]}],"stream":true}` {
		t.Fatal("original body should not be mutated")
	}
	if inst := gjson.GetBytes(got, "instructions").String(); inst != "group-policy\n\ncodex-base" {
		t.Fatalf("instructions not prepended exactly: %q", inst)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "long-context" {
		t.Fatalf("input context changed: %q", text)
	}
}

func TestApplyGroupSystemPromptToResponsesBodyModes(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want string
	}{
		{name: "prepend default", want: "group\n\nbase"},
		{name: "append", mode: "append", want: "base\n\ngroup"},
		{name: "replace", mode: "replace", want: "group"},
	}
	for _, tc := range tests {
		body := []byte(`{"instructions":"base","input":[]}`)
		group := &domain.Group{SystemPrompt: "group", SystemPromptMode: tc.mode}
		got := applyGroupSystemPromptToResponsesBody(body, group)
		if inst := gjson.GetBytes(got, "instructions").String(); inst != tc.want {
			t.Fatalf("%s instructions: got %q want %q", tc.name, inst, tc.want)
		}
	}
}

func TestApplyGroupSystemPromptToResponsesBodyInsertsMissingInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}]}`)
	group := &domain.Group{SystemPrompt: "group"}

	got := applyGroupSystemPromptToResponsesBody(body, group)

	if !gjson.ValidBytes(got) {
		t.Fatalf("invalid JSON after insertion: %s", got)
	}
	if inst := gjson.GetBytes(got, "instructions").String(); inst != "group" {
		t.Fatalf("instructions not inserted: %q", inst)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "keep" {
		t.Fatalf("input context changed: %q", text)
	}
}

func TestResponsesPassthroughScrubsClientMetadataBeforeRawInvoke(t *testing.T) {
	cfg := &config.Root{}
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.HeadBuffer.MaxBytes = 4096
	cfg.Scheduler.Failover.HeadBuffer.MaxEvents = 2
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "raw",
		State:    domain.StateActive,
	})
	raw := &captureRawProvider{}
	reg := provider.NewRegistry()
	reg.Register(raw)
	gw := NewGateway(Deps{
		Cfg:       cfg,
		Sched:     sched,
		Providers: reg,
	})

	group := &domain.Group{
		ID:           "g",
		TenantID:     "default",
		Provider:     "raw",
		AccountIDs:   []string{"acc-1"},
		SystemPrompt: "group-policy",
	}
	body := `{"model":"gpt-5.5","instructions":"codex-base","prompt_cache_key":"thread-stable-123","x-request-id":"rid","metadata":{"session_id":"sid","client_id":"cid","keep":"ok"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"reasoning":{"effort":"xhigh","summary":"detailed"},"service_tier":"priority","max_output_tokens":200000,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"include":["reasoning.encrypted_content"],"parallel_tool_calls":true,"previous_response_id":"resp-prev","store":false,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(raw.body) == 0 {
		t.Fatal("raw provider did not receive body")
	}
	if bytes.Contains(raw.body, []byte(`"x-request-id"`)) ||
		bytes.Contains(raw.body, []byte(`"session_id"`)) ||
		bytes.Contains(raw.body, []byte(`"client_id"`)) {
		t.Fatalf("client metadata was not scrubbed: %s", raw.body)
	}
	if !bytes.Contains(raw.body, []byte(`"keep":"ok"`)) {
		t.Fatalf("unrelated metadata should be preserved: %s", raw.body)
	}
	if !bytes.Contains(raw.body, []byte(`"input"`)) {
		t.Fatalf("context input should be preserved: %s", raw.body)
	}
	if inst := gjson.GetBytes(raw.body, "instructions").String(); inst != "group-policy\n\ncodex-base" {
		t.Fatalf("group system prompt was not applied to raw Responses instructions: %q", inst)
	}
	for path, want := range map[string]string{
		"model":                "gpt-5.5",
		"prompt_cache_key":     "thread-stable-123",
		"reasoning.effort":     "xhigh",
		"reasoning.summary":    "detailed",
		"service_tier":         "priority",
		"previous_response_id": "resp-prev",
	} {
		if got := gjson.GetBytes(raw.body, path).String(); got != want {
			t.Fatalf("%s changed: got %q want %q body=%s", path, got, want, raw.body)
		}
	}
	if got := gjson.GetBytes(raw.body, "max_output_tokens").Int(); got != 200000 {
		t.Fatalf("max_output_tokens changed: got %d", got)
	}
	if !gjson.GetBytes(raw.body, "parallel_tool_calls").Bool() {
		t.Fatalf("parallel_tool_calls should be preserved: %s", raw.body)
	}
	if gjson.GetBytes(raw.body, "store").Bool() {
		t.Fatalf("store=false should be preserved: %s", raw.body)
	}
	if got := gjson.GetBytes(raw.body, "include.0").String(); got != "reasoning.encrypted_content" {
		t.Fatalf("include changed: %q body=%s", got, raw.body)
	}
	if got := gjson.GetBytes(raw.body, "tools.0.name").String(); got != "shell" {
		t.Fatalf("tools changed: %q body=%s", got, raw.body)
	}
}

func TestResponsesPassthroughPinsUnknownPreviousResponseWithoutReplayTranscript(t *testing.T) {
	cfg := &config.Root{}
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.HeadBuffer.MaxBytes = 4096
	cfg.Scheduler.Failover.HeadBuffer.MaxEvents = 2
	sched := scheduler.New(cfg.Scheduler)
	for _, id := range []string{"acc-1", "acc-2"} {
		sched.Register(&domain.Account{
			ID:       id,
			TenantID: "default",
			Provider: "raw",
			State:    domain.StateActive,
		})
	}
	raw := &captureRawProvider{responseIDs: []string{"resp-first", "resp-second"}}
	reg := provider.NewRegistry()
	reg.Register(raw)
	gw := NewGateway(Deps{
		Cfg:       cfg,
		Sched:     sched,
		Providers: reg,
	})

	group := &domain.Group{
		ID:         "g",
		TenantID:   "default",
		Provider:   "raw",
		AccountIDs: []string{"acc-2"},
	}
	body := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable-abc","previous_response_id":"upstream-only-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}],"stream":true}`
	gw.responses.RecordThread(responsesThreadKey([]byte(body), group), "acc-1")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(raw.calls) != 1 {
		t.Fatalf("expected one raw call, got %d", len(raw.calls))
	}
	if raw.calls[0].accountID != "acc-1" {
		t.Fatalf("unknown previous_response_id without replay transcript should stay on thread account acc-1, got %s", raw.calls[0].accountID)
	}
	if got := gjson.GetBytes(raw.calls[0].body, "previous_response_id").String(); got != "upstream-only-prev" {
		t.Fatalf("previous_response_id should be preserved when staying on original account, got %q body=%s", got, raw.calls[0].body)
	}
	got, ok := gw.responses.Lookup("resp-first")
	if !ok {
		t.Fatal("expected lightweight response affinity")
	}
	if got.AccountID != "acc-1" {
		t.Fatalf("response affinity account changed: %s", got.AccountID)
	}
	if len(got.Transcript) != 0 {
		t.Fatalf("unknown previous_response_id should not store an incomplete replay transcript: %d items", len(got.Transcript))
	}
}

func TestResponsesPassthroughReplaysThreadTranscriptWhenPreviousMappingMissingAndPinnedAccountExhausted(t *testing.T) {
	cfg := &config.Root{}
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.HeadBuffer.MaxBytes = 4096
	cfg.Scheduler.Failover.HeadBuffer.MaxEvents = 2
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "raw",
		State:    domain.StateActive,
		Quota: domain.QuotaState{
			ShortWindow: domain.QuotaWindow{Limit: 100, Used: 100, Confidence: 1},
			LongWindow:  domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
		},
	})
	sched.Register(&domain.Account{
		ID:       "acc-2",
		TenantID: "default",
		Provider: "raw",
		State:    domain.StateActive,
		Quota: domain.QuotaState{
			ShortWindow: domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
			LongWindow:  domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
		},
	})
	raw := &captureRawProvider{responseIDs: []string{"resp-migrated"}}
	reg := provider.NewRegistry()
	reg.Register(raw)
	gw := NewGateway(Deps{
		Cfg:       cfg,
		Sched:     sched,
		Providers: reg,
	})

	group := &domain.Group{
		ID:         "g",
		TenantID:   "default",
		Provider:   "raw",
		AccountIDs: []string{"acc-1", "acc-2"},
	}
	body := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable-abc","previous_response_id":"upstream-only-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}],"stream":true}`
	threadKey := responsesThreadKey([]byte(body), group)
	gw.responses.RecordWithThread("thread-latest", "acc-1", threadKey, []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(raw.calls) != 1 {
		t.Fatalf("expected one raw call, got %d", len(raw.calls))
	}
	if raw.calls[0].accountID != "acc-2" {
		t.Fatalf("exhausted pinned account should replay on healthy acc-2, got %s", raw.calls[0].accountID)
	}
	if gjson.GetBytes(raw.calls[0].body, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed for replay body: %s", raw.calls[0].body)
	}
	if got := gjson.GetBytes(raw.calls[0].body, "input.#").Int(); got != 3 {
		t.Fatalf("replay body should contain previous transcript plus current input, got %d items body=%s", got, raw.calls[0].body)
	}
	got, ok := gw.responses.Lookup("resp-migrated")
	if !ok {
		t.Fatal("expected migrated response affinity")
	}
	if got.AccountID != "acc-2" {
		t.Fatalf("migrated response should bind to acc-2, got %s", got.AccountID)
	}
	if len(got.Transcript) != 3 {
		t.Fatalf("migrated response should retain replay transcript, got %d items", len(got.Transcript))
	}
	thread, ok := gw.responses.LookupThreadTranscript(threadKey)
	if !ok {
		t.Fatal("expected updated thread transcript")
	}
	if thread.AccountID != "acc-2" || thread.ResponseID != "resp-migrated" {
		t.Fatalf("thread should migrate to acc-2/new response id, got %+v", thread)
	}
}

func TestResponsesPassthroughReplaysThreadTranscriptWhenPinnedAccountDraining(t *testing.T) {
	cfg := &config.Root{}
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Quota.DrainThreshold = 0.10
	cfg.Scheduler.Failover.HeadBuffer.MaxBytes = 4096
	cfg.Scheduler.Failover.HeadBuffer.MaxEvents = 2
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "raw",
		State:    domain.StateActive,
		Quota: domain.QuotaState{
			ShortWindow: domain.QuotaWindow{Limit: 100, Used: 95, Confidence: 1},
			LongWindow:  domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
		},
	})
	sched.Register(&domain.Account{
		ID:       "acc-2",
		TenantID: "default",
		Provider: "raw",
		State:    domain.StateActive,
		Quota: domain.QuotaState{
			ShortWindow: domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
			LongWindow:  domain.QuotaWindow{Limit: 100, Used: 1, Confidence: 1},
		},
	})
	raw := &captureRawProvider{responseIDs: []string{"resp-migrated"}}
	reg := provider.NewRegistry()
	reg.Register(raw)
	gw := NewGateway(Deps{
		Cfg:       cfg,
		Sched:     sched,
		Providers: reg,
	})

	group := &domain.Group{
		ID:         "g",
		TenantID:   "default",
		Provider:   "raw",
		AccountIDs: []string{"acc-1", "acc-2"},
	}
	body := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable-abc","previous_response_id":"upstream-only-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}],"stream":true}`
	threadKey := responsesThreadKey([]byte(body), group)
	gw.responses.RecordWithThread("thread-latest", "acc-1", threadKey, []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(raw.calls) != 1 {
		t.Fatalf("expected one raw call, got %d", len(raw.calls))
	}
	if raw.calls[0].accountID != "acc-2" {
		t.Fatalf("draining pinned account should replay on healthy acc-2, got %s", raw.calls[0].accountID)
	}
	if gjson.GetBytes(raw.calls[0].body, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed for replay body: %s", raw.calls[0].body)
	}
	if got := gjson.GetBytes(raw.calls[0].body, "input.#").Int(); got != 3 {
		t.Fatalf("replay body should contain previous transcript plus current input, got %d items body=%s", got, raw.calls[0].body)
	}
}

func TestResponsesThreadKeyRejectsUnsafePromptCacheKey(t *testing.T) {
	group := &domain.Group{ID: "g", TenantID: "default", Provider: "raw"}
	if got := responsesThreadKey([]byte(`{"prompt_cache_key":"bad\nkey"}`), group); got != "" {
		t.Fatalf("unsafe prompt cache key should be ignored, got %q", got)
	}
	if got := responsesThreadKey([]byte(`{"prompt_cache_key":"thread-ok"}`), group); got == "" {
		t.Fatal("safe prompt cache key should produce a thread key")
	}
}

type captureRawCall struct {
	accountID string
	body      []byte
}

type captureRawProvider struct {
	body        []byte
	calls       []captureRawCall
	responseIDs []string
}

func (p *captureRawProvider) Name() string { return "raw" }

func (p *captureRawProvider) Invoke(context.Context, *domain.Account, *ir.Request) (<-chan ir.Event, error) {
	out := make(chan ir.Event)
	close(out)
	return out, nil
}

func (p *captureRawProvider) Probe(context.Context, *domain.Account) error { return nil }

func (p *captureRawProvider) Discover(context.Context, *domain.Account) (*domain.QuotaState, error) {
	return &domain.QuotaState{}, nil
}

func (p *captureRawProvider) InvokeRaw(_ interface{}, accountID string, body []byte) (io.ReadCloser, int, error) {
	p.body = append([]byte(nil), body...)
	p.calls = append(p.calls, captureRawCall{accountID: accountID, body: append([]byte(nil), body...)})
	responseID := "resp-1"
	if len(p.responseIDs) > 0 {
		responseID = p.responseIDs[0]
		p.responseIDs = p.responseIDs[1:]
	}
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	return io.NopCloser(strings.NewReader(sse)), http.StatusOK, nil
}
