package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/stealth"
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

func TestResponsesThreadOnceSkipsKnownThreadAndReinjectsAfterCompact(t *testing.T) {
	cfg := &config.Root{}
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.HeadBuffer.MaxBytes = 4096
	cfg.Scheduler.Failover.HeadBuffer.MaxEvents = 2
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "cyber",
		Provider: "chatgpt",
		State:    domain.StateActive,
	})
	raw := &captureRawProvider{name: "chatgpt", responseIDs: []string{"resp-1", "resp-2", "resp-3"}}
	reg := provider.NewRegistry()
	reg.Register(raw)
	gw := NewGateway(Deps{
		Cfg:       cfg,
		Sched:     sched,
		Providers: reg,
	})
	group := &domain.Group{
		ID:                    "openai_cyber",
		TenantID:              "cyber",
		Provider:              "chatgpt",
		AccountIDs:            []string{"acc-1"},
		SystemPrompt:          "group-policy",
		SystemPromptInjection: "thread_once",
	}

	firstBody := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable","instructions":"codex-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()
	gw.handleResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status: %d body=%s", rec.Code, rec.Body.String())
	}
	if inst := gjson.GetBytes(raw.calls[0].body, "instructions").String(); inst != "group-policy\n\ncodex-base" {
		t.Fatalf("first request should inject group prompt, got %q", inst)
	}

	secondBody := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable","previous_response_id":"resp-1","instructions":"codex-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}],"stream":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondBody))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec = httptest.NewRecorder()
	gw.handleResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status: %d body=%s", rec.Code, rec.Body.String())
	}
	if inst := gjson.GetBytes(raw.calls[1].body, "instructions").String(); inst != "codex-base" {
		t.Fatalf("known thread should not repeat group prompt, got %q", inst)
	}

	compactBody := []byte(`{"model":"gpt-5.5","prompt_cache_key":"thread-stable","previous_response_id":"resp-2","instructions":"compact-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"compact"}]}],"stream":false}`)
	gw.requireResponsesSystemPromptAfterCompact(compactBody, group)

	thirdBody := `{"model":"gpt-5.5","prompt_cache_key":"thread-stable","previous_response_id":"resp-2","instructions":"codex-base","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"after compact"}]}],"stream":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(thirdBody))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec = httptest.NewRecorder()
	gw.handleResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third status: %d body=%s", rec.Code, rec.Body.String())
	}
	if inst := gjson.GetBytes(raw.calls[2].body, "instructions").String(); inst != "group-policy\n\ncodex-base" {
		t.Fatalf("first request after compact should reinject group prompt, got %q", inst)
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

func TestBackendAPICodexResponsesRouteUsesRawPassthrough(t *testing.T) {
	cfg := &config.Root{
		Groups: []config.Group{{
			ID:         "g",
			TenantID:   "default",
			Provider:   "raw",
			APIKeys:    []string{"sk-test"},
			AccountIDs: []string{"acc-1"},
		}},
	}
	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
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
		Resolver:  resolver,
		Sched:     sched,
		Providers: reg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	body := `{"model":"gpt-5.5","input":[],"reasoning":{"effort":"high"},"service_tier":"priority","max_output_tokens":200000,"store":false,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(raw.body) == 0 {
		t.Fatal("raw provider did not receive backend-api/codex/responses request")
	}
	for path, want := range map[string]string{
		"model":            "gpt-5.5",
		"reasoning.effort": "high",
		"service_tier":     "priority",
	} {
		if got := gjson.GetBytes(raw.body, path).String(); got != want {
			t.Fatalf("%s changed: got %q want %q body=%s", path, got, want, raw.body)
		}
	}
	if got := gjson.GetBytes(raw.body, "max_output_tokens").Int(); got != 200000 {
		t.Fatalf("max_output_tokens changed: got %d body=%s", got, raw.body)
	}
}

func TestResponsesPassthroughUsesPersistedIdentityRewrite(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "identity.json")
	baseBundle := stealth.IdentityBundle{
		Username:        "root",
		Hostname:        "vps-alpha",
		Platform:        "linux",
		OS:              "linux",
		OSVersion:       "6.1",
		Arch:            "amd64",
		Shell:           "/bin/bash",
		Terminal:        "xterm-256color",
		Runtime:         "node",
		RuntimeVersion:  "v24.3.0",
		AppVersion:      "2.1.138",
		IPAddress:       "10.0.0.10",
		ASN:             "AS64512",
		Region:          "us-east",
		Timezone:        "UTC",
		DNSResolvers:    []string{"10.0.0.53", "1.1.1.1"},
		DNSProvider:     "vps-dns",
		StatsigStableID: "stable_base",
		StatsigUserID:   "statsig_user_base",
		FeatureGateSeed: "fgseed_base",
		FeatureGates: map[string]bool{
			"claude_code_plan_mode": true,
		},
		ExperimentGroups: map[string]string{
			"claude_code_ui": "variant_a",
		},
		UserID:           "user_vps",
		SessionID:        "sess_vps",
		AccountID:        "acct_vps",
		AccountUUID:      "00000000-0000-0000-0000-000000000001",
		OrganizationID:   "org_vps",
		OrganizationUUID: "00000000-0000-0000-0000-000000000002",
		Email:            "root@vps-alpha.local",
	}
	identityPayload, _ := json.Marshal(map[string]any{
		"version": 1,
		"bundle":  baseBundle,
	})
	if err := os.WriteFile(identityPath, identityPayload, 0600); err != nil {
		t.Fatalf("write identity fixture: %v", err)
	}
	wantBundle, err := stealth.LoadOrCreateAccountIdentityBundle(identityPath, "raw", "acc-1", "")
	if err != nil {
		t.Fatalf("create account identity fixture: %v", err)
	}

	cfg := &config.Root{}
	cfg.Stealth.IdentityRewrite = true
	cfg.Stealth.IdentityPath = identityPath
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
		ID:         "g",
		TenantID:   "default",
		Provider:   "raw",
		AccountIDs: []string{"acc-1"},
	}
	body := `{"model":"gpt-5.5","session_id":"client-session","user":{"id":"client-user","email":"alice@example.test","account_uuid":"client-account"},"organization":{"id":"client-org"},"cwd":"/Users/alice/private/app","workspace":{"host_paths":["/Users/alice/private/app"]},"terminal":{"type":"iTerm.app"},"app":{"version":"0.0.1"},"platform":"darwin","os":{"type":"darwin"},"host":{"arch":"arm64","name":"alice-mbp.local"},"ip_address":"192.0.2.44","dns":{"provider":"google","resolvers":["8.8.8.8"]},"statsig":{"stable_id":"local-stable","user_id":"local-statsig-user","feature_gates":{"claude_code_plan_mode":false}},"feature":{"gate":{"seed":"local-seed"}},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"cwd=/Users/alice/private/app hostname=alice-mbp.local"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	for _, leaked := range []string{
		"client-session", "client-user", "client-account", "client-org",
		"alice-mbp.local", "iTerm.app", `"version":"0.0.1"`, `"darwin"`, `"arm64"`,
		"192.0.2.44", "8.8.8.8", "google", "local-stable", "local-statsig-user", "local-seed",
	} {
		if bytes.Contains(raw.body, []byte(leaked)) {
			t.Fatalf("client identity leaked into raw provider body via %q: %s", leaked, raw.body)
		}
	}
	for path, want := range map[string]string{
		"session_id":        wantBundle.SessionID,
		"user.id":           wantBundle.UserID,
		"user.email":        wantBundle.Email,
		"user.account_uuid": wantBundle.AccountUUID,
		"organization.id":   wantBundle.OrganizationID,
		"cwd":               "/Users/alice/private/app",
		"terminal.type":     wantBundle.Terminal,
		"app.version":       wantBundle.AppVersion,
		"platform":          wantBundle.Platform,
		"os.type":           wantBundle.OS,
		"host.arch":         wantBundle.Arch,
		"host.name":         wantBundle.Hostname,
		"ip_address":        wantBundle.IPAddress,
		"dns.provider":      wantBundle.DNSProvider,
		"statsig.stable_id": wantBundle.StatsigStableID,
		"statsig.user_id":   wantBundle.StatsigUserID,
		"feature.gate.seed": wantBundle.FeatureGateSeed,
	} {
		if got := gjson.GetBytes(raw.body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q body=%s", path, got, want, raw.body)
		}
	}
	if gotPath := gjson.GetBytes(raw.body, "workspace.host_paths.0").String(); gotPath != "/Users/alice/private/app" {
		t.Fatalf("workspace.host_paths should be preserved, got %q body=%s", gotPath, raw.body)
	}
	if gotResolver := gjson.GetBytes(raw.body, "dns.resolvers.0").String(); gotResolver != "10.0.0.53" {
		t.Fatalf("dns resolver not rewritten: got %q body=%s", gotResolver, raw.body)
	}
	if text := gjson.GetBytes(raw.body, "input.0.content.0.text").String(); !strings.Contains(text, "/Users/alice/private/app") || !strings.Contains(text, wantBundle.Hostname) {
		t.Fatalf("input text was not identity rewritten: %q body=%s", text, raw.body)
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

func TestResponsesPassthroughAuthFailureReturns401Not502AndDoesNotBan(t *testing.T) {
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
	raw := &captureRawProvider{err: errors.New(`refresh after token_invalidated: refresh 401: {"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","code":"refresh_token_reused"}}`)}
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
		AccountIDs: []string{"acc-1"},
	}
	body := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxResolved, auth.Resolved{Group: group, APIKey: "sk-test"}))
	rec := httptest.NewRecorder()

	gw.handleResponses(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "auth_failed" {
		t.Fatalf("error type = %q, want auth_failed body=%s", got, rec.Body.String())
	}
	acc, ok := sched.AccountByID("acc-1")
	if !ok {
		t.Fatal("auth failed account should remain in scheduler")
	}
	if acc.State == domain.StateBanned {
		t.Fatal("auth failed account should not be marked banned")
	}
	if ids := sched.BannedAccountIDs(); len(ids) != 0 {
		t.Fatalf("auth failed account should not be in banned list: %#v", ids)
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
	name        string
	body        []byte
	calls       []captureRawCall
	responseIDs []string
	err         error
}

func (p *captureRawProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "raw"
}

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
	if p.err != nil {
		return nil, 0, p.err
	}
	responseID := "resp-1"
	if len(p.responseIDs) > 0 {
		responseID = p.responseIDs[0]
		p.responseIDs = p.responseIDs[1:]
	}
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	return io.NopCloser(strings.NewReader(sse)), http.StatusOK, nil
}
