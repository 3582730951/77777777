package chatgpt

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

func TestParseWhamUsagePrimarySecondaryWindows(t *testing.T) {
	body := []byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 12.5, "limit_window_seconds": 18000, "reset_at": 1777722657},
			"secondary_window": {"used_percent": 87, "limit_window_seconds": 604800, "reset_at": 1778123456}
		},
		"plan_type": "plus",
		"credits": {"has_credits": true, "balance": "$4.20"}
	}`)
	state := &domain.QuotaState{}

	parseWhamUsage(body, state)

	if state.ShortWindow.Limit != 100 || state.ShortWindow.Used != 12.5 || state.ShortWindow.Confidence != 1 {
		t.Fatalf("bad short window: %+v", state.ShortWindow)
	}
	if state.LongWindow.Limit != 100 || state.LongWindow.Used != 87 || state.LongWindow.Confidence != 1 {
		t.Fatalf("bad long window: %+v", state.LongWindow)
	}
	if state.TierWindows["five_hour"].Used != 12.5 {
		t.Fatalf("missing five_hour tier: %+v", state.TierWindows)
	}
	if state.TierWindows["seven_day"].Used != 87 {
		t.Fatalf("missing seven_day tier: %+v", state.TierWindows)
	}
	if state.PlanTier != "plus" {
		t.Fatalf("plan not parsed: %q", state.PlanTier)
	}
	if state.ExtraUsage == nil || !state.ExtraUsage.IsEnabled || state.ExtraUsage.Currency != "$4.20" {
		t.Fatalf("credits not parsed: %+v", state.ExtraUsage)
	}
	if state.ShortWindow.ResetAt.IsZero() || state.LongWindow.ResetAt.IsZero() {
		t.Fatal("reset timestamps should be parsed")
	}
}

func TestParseWhamUsageLegacyWindowMinutes(t *testing.T) {
	body := []byte(`{
		"rate_limit": {"used_percent": 25, "window_minutes": 300, "resets_at": 1777722657},
		"additional_rate_limits": [
			{"limit_name": "weekly", "rate_limit": {"used_percent": 40, "window_minutes": 10080, "resets_at": 1778123456}}
		]
	}`)
	state := &domain.QuotaState{}

	parseWhamUsage(body, state)

	if state.ShortWindow.Used != 25 || state.ShortWindow.Limit != 100 {
		t.Fatalf("legacy short window not parsed: %+v", state.ShortWindow)
	}
	if state.TierWindows["five_hour"].Used != 25 {
		t.Fatalf("legacy five_hour tier not parsed: %+v", state.TierWindows)
	}
	if state.TierWindows["weekly"].Used != 40 {
		t.Fatalf("additional weekly tier not parsed: %+v", state.TierWindows)
	}
}

func TestParseWhamUsageExtraRateLimitShapes(t *testing.T) {
	body := []byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 5, "limit_window_seconds": 18000, "reset_at": 1777722657},
			"secondary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_at": 1778123456}
		},
		"gpt5_rate_limit": {
			"metered_feature": "gpt_5",
			"rate_limit": {
				"primary_window": {"used_percent": 11, "limit_window_seconds": 18000, "reset_at": 1777722657},
				"secondary_window": {"used_percent": 22, "limit_window_seconds": 604800, "reset_at": 1778123456}
			}
		},
		"additional_rate_limits": {
			"images": {
				"limit_name": "image_generation",
				"rate_limit": {
					"primary_window": {"used_percent": 33, "limit_window_seconds": 86400, "reset_at": 1777722657}
				}
			}
		}
	}`)
	state := &domain.QuotaState{}

	parseWhamUsage(body, state)

	if state.TierWindows["gpt_5_primary"].Used != 11 {
		t.Fatalf("extra primary tier not parsed: %+v", state.TierWindows)
	}
	if state.TierWindows["gpt_5_secondary"].Used != 22 {
		t.Fatalf("extra secondary tier not parsed: %+v", state.TierWindows)
	}
	if state.TierWindows["image_generation_primary"].Used != 33 {
		t.Fatalf("object additional tier not parsed: %+v", state.TierWindows)
	}
}

func TestFetchWhamUsageTreatsBanSignalAsBanned(t *testing.T) {
	p := New(ModeReal)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"account_deactivated","message":"account has been deactivated"}}`))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	_, err := p.fetchWhamUsage(context.Background(), &domain.Account{}, "tok", "acct")
	if err == nil {
		t.Fatal("expected banned error")
	}
	if !isBannedError(err) {
		t.Fatalf("expected banned classification, got %v", err)
	}
}

func TestFetchWhamUsageUsesCodexHeaders(t *testing.T) {
	p := New(ModeReal)
	var gotUA, gotOriginator, gotAccount string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotOriginator = r.Header.Get("originator")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":1777722657},"secondary_window":{"used_percent":2,"limit_window_seconds":604800,"reset_at":1778123456}}}`))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	state, err := p.fetchWhamUsage(context.Background(), &domain.Account{UA: "codex-cli-test"}, "tok", "acct-1")
	if err != nil {
		t.Fatalf("fetch wham: %v", err)
	}
	if state.ShortWindow.Used != 1 || state.LongWindow.Used != 2 {
		t.Fatalf("bad quota: %+v", state)
	}
	if gotUA != "codex-cli-test" || gotOriginator != "codex_cli_rs" || gotAccount != "acct-1" {
		t.Fatalf("headers not preserved: ua=%q originator=%q account=%q", gotUA, gotOriginator, gotAccount)
	}
}

func TestInvokeRawUsesPromptCacheKeyForCodexSessionHeaders(t *testing.T) {
	p, cleanup := newRawInvokeTestProvider(t)
	defer cleanup()

	wantBody := []byte(`{"model":"gpt-5.5","prompt_cache_key":"thread-stable-123","input":[],"reasoning":{"effort":"xhigh"},"store":false,"stream":true}`)
	var gotBody []byte
	var gotSessionID, gotThreadID, gotRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotSessionID = r.Header.Get("session_id")
		gotThreadID = r.Header.Get("thread_id")
		gotRequestID = r.Header.Get("x-client-request-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	rc, status, err := p.InvokeRaw(context.Background(), "acc-raw", wantBody)
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if gotSessionID != "thread-stable-123" || gotThreadID != "thread-stable-123" || gotRequestID != "thread-stable-123" {
		t.Fatalf("session headers not derived from prompt_cache_key: session=%q thread=%q request=%q", gotSessionID, gotThreadID, gotRequestID)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("raw body changed:\n got %s\nwant %s", gotBody, wantBody)
	}
}

func TestInvokeRawFallsBackToRandomSessionWithoutPromptCacheKey(t *testing.T) {
	p, cleanup := newRawInvokeTestProvider(t)
	defer cleanup()

	var gotSessionID, gotThreadID, gotRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = r.Header.Get("session_id")
		gotThreadID = r.Header.Get("thread_id")
		gotRequestID = r.Header.Get("x-client-request-id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	rc, status, err := p.InvokeRaw(context.Background(), "acc-raw", []byte(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if gotSessionID == "" {
		t.Fatal("session_id fallback should be set")
	}
	if gotThreadID != "" || gotRequestID != "" {
		t.Fatalf("thread headers should not be invented without prompt_cache_key: thread=%q request=%q", gotThreadID, gotRequestID)
	}
}

func TestCodexSessionHeadersRejectUnsafePromptCacheKey(t *testing.T) {
	sessionID, threadID := codexSessionHeadersFromResponsesBody([]byte("{\"prompt_cache_key\":\"bad\\nkey\"}"))
	if sessionID == "" || sessionID == "bad\nkey" {
		t.Fatalf("unsafe prompt_cache_key should fall back to a generated session id, got %q", sessionID)
	}
	if threadID != "" {
		t.Fatalf("unsafe prompt_cache_key should not produce thread header, got %q", threadID)
	}
}

func newRawInvokeTestProvider(t *testing.T) (*Provider, func()) {
	t.Helper()
	p := New(ModeReal)
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(dbPath, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	acc := &domain.Account{
		ID:       "acc-raw",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	secret := store.AccountSecret{
		SessionToken: `{"accessToken":"access-token","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"}}`,
	}
	if err := st.UpsertAccount(context.Background(), acc, secret); err != nil {
		_ = st.Close()
		t.Fatalf("upsert account: %v", err)
	}
	p.SetStore(st)
	return p, func() { _ = st.Close() }
}

func rewriteTransportClient(base string) *http.Client {
	target := base
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			next := req.Clone(req.Context())
			u := *req.URL
			baseReq, _ := http.NewRequest(req.Method, target, nil)
			u.Scheme = baseReq.URL.Scheme
			u.Host = baseReq.URL.Host
			next.URL = &u
			next.RequestURI = ""
			return http.DefaultTransport.RoundTrip(next)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
