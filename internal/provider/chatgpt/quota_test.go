package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
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

func TestDiscoverPrefersLivePlanTierAndPersistsIt(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	p.SetStore(st)

	acc := &domain.Account{
		ID:       "acc-plan",
		TenantID: "default",
		Provider: "chatgpt",
		PlanTier: "free",
		State:    domain.StateActive,
	}
	secret := store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  testJWTExp(time.Now().Add(time.Hour)),
			RefreshToken: "rt",
			Expires:      time.Now().Add(time.Hour),
			AccountID:    "chatgpt-account",
			PlanType:     "free",
		}),
		RefreshToken: "rt",
	}
	if err := st.UpsertAccount(ctx, acc, secret); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"rate_limit": {
				"primary_window": {"used_percent": 1, "limit_window_seconds": 18000, "reset_at": 1777722657},
				"secondary_window": {"used_percent": 2, "limit_window_seconds": 604800, "reset_at": 1778123456}
			},
			"plan_type": "plus"
		}`))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	state, err := p.Discover(ctx, acc)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if state.PlanTier != "plus" {
		t.Fatalf("state plan tier = %q, want plus", state.PlanTier)
	}
	if acc.PlanTier != "plus" {
		t.Fatalf("account plan tier = %q, want plus", acc.PlanTier)
	}
	stored, err := st.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get stored account: %v", err)
	}
	if stored.PlanTier != "plus" {
		t.Fatalf("stored plan tier = %q, want plus", stored.PlanTier)
	}
	if len(state.DiscoveredModels) == 0 {
		t.Fatal("expected discovered models")
	}
	var hasSpark bool
	for _, model := range state.DiscoveredModels {
		if model.ID == "gpt-5.3-codex-spark" {
			hasSpark = true
			break
		}
	}
	if !hasSpark {
		t.Fatal("plus live plan should include spark model")
	}
}

func TestDiscoverRefreshesTokenInvalidatedWhamUsage(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	p.SetStore(st)

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-token-invalidated-discover",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  oldAccess,
			RefreshToken: "rt-old",
			Expires:      time.Now().Add(time.Hour),
			AccountID:    "chatgpt-account",
			PlanType:     "plus",
		}),
		RefreshToken: "rt-old",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	var calls int
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		calls++
		auths = append(auths, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+newAccess {
			t.Fatalf("retry authorization = %q, want refreshed token", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{
			"rate_limit": {
				"primary_window": {"used_percent": 3, "limit_window_seconds": 18000, "reset_at": 1777722657},
				"secondary_window": {"used_percent": 4, "limit_window_seconds": 604800, "reset_at": 1778123456}
			},
			"plan_type": "plus"
		}`))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	state, err := p.Discover(ctx, acc)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if len(auths) != 2 || auths[0] != "Bearer "+oldAccess || auths[1] != "Bearer "+newAccess {
		t.Fatalf("authorization sequence = %#v", auths)
	}
	if state.ShortWindow.Used != 3 || state.LongWindow.Used != 4 || state.PlanTier != "plus" {
		t.Fatalf("unexpected quota state: %+v", state)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
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

func TestInvokeRawNormalizesFastServiceTierAlias(t *testing.T) {
	p, cleanup := newRawInvokeTestProvider(t)
	defer cleanup()

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	rc, status, err := p.InvokeRaw(context.Background(), "acc-raw", []byte(`{"model":"gpt-5.5","service_tier":" fast ","input":[]}`))
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := gjson.GetBytes(gotBody, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority; body=%s", got, gotBody)
	}
}

func TestInvokeRawRefreshesAndRetriesTokenInvalidated(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-token-invalidated-raw",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	secret := store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  oldAccess,
			RefreshToken: "rt-old",
			Expires:      time.Now().Add(time.Hour),
			AccountID:    "chatgpt-account",
			PlanType:     "plus",
			Email:        "user@example.com",
			IDToken:      "id-old",
		}),
		RefreshToken: "rt-old",
	}
	if err := st.UpsertAccount(ctx, acc, secret); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	var calls int
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auths = append(auths, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+newAccess {
			t.Fatalf("retry authorization = %q, want refreshed token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	rc, status, err := p.InvokeRaw(ctx, acc.ID, []byte(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if len(auths) != 2 || auths[0] != "Bearer "+oldAccess || auths[1] != "Bearer "+newAccess {
		t.Fatalf("authorization sequence = %#v", auths)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatal("stored access token was not refreshed")
	}
}

func TestInvokeRawRecoversTokenInvalidatedViaStoredCookieWhenRefreshTokenReused(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-token-invalidated-cookie",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  oldAccess,
			RefreshToken: "rt-used",
			Expires:      time.Now().Add(time.Hour),
			AccountID:    "chatgpt-account",
			PlanType:     "plus",
			Email:        "user@example.com",
			IDToken:      "id-old",
		}),
		RefreshToken: "rt-used",
		Cookies:      []byte(`__Secure-next-auth.session-token=fresh-cookie`),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	p.SetStore(st)
	var refreshCalls int
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		refreshCalls++
		if refreshToken != "rt-used" {
			t.Fatalf("refresh token = %q, want rt-used", refreshToken)
		}
		return "", "", "", 0, errors.New("refresh 401: refresh_token_reused")
	})

	var codexCalls, sessionCalls int
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			sessionCalls++
			if got := r.Header.Get("Cookie"); got != "__Secure-next-auth.session-token=fresh-cookie" {
				t.Fatalf("session cookie = %q, want next-auth cookie", got)
			}
			_, _ = w.Write([]byte(`{"accessToken":"` + newAccess + `","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"},"user":{"email":"user@example.com"}}`))
		case "/backend-api/codex/responses":
			codexCalls++
			auths = append(auths, r.Header.Get("Authorization"))
			if codexCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null}}`))
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+newAccess {
				t.Fatalf("retry authorization = %q, want cookie-recovered token", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := rewriteTransportClient(server.URL)
	p.httpClient = client
	p.resolver.httpClient = client

	rc, status, err := p.InvokeRaw(ctx, acc.ID, []byte(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		body, _ := io.ReadAll(rc)
		t.Fatalf("status = %d body=%s", status, body)
	}
	if refreshCalls != 1 || sessionCalls != 1 || codexCalls != 2 {
		t.Fatalf("calls refresh=%d session=%d codex=%d, want 1/1/2", refreshCalls, sessionCalls, codexCalls)
	}
	if len(auths) != 2 || auths[0] != "Bearer "+oldAccess || auths[1] != "Bearer "+newAccess {
		t.Fatalf("authorization sequence = %#v", auths)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatalf("stored access token = %q, want cookie-recovered token", parsed.AccessToken)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" {
		t.Fatalf("reused refresh token should be cleared from persisted credentials: session=%q top=%q", parsed.RefreshToken, sec.RefreshToken)
	}
}

func TestInvokeRawRecoversTokenInvalidatedViaStoredCookieWhenNoRefreshToken(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-token-invalidated-cookie-no-refresh",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken: oldAccess,
			Expires:     time.Now().Add(time.Hour),
			AccountID:   "chatgpt-account",
			PlanType:    "plus",
			Email:       "user@example.com",
			IDToken:     "id-old",
		}),
		Cookies: []byte(`__Secure-next-auth.session-token=fresh-cookie`),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		t.Fatalf("refresh callback should not be called without a refresh_token; got %q", refreshToken)
		return "", "", "", 0, nil
	})

	var codexCalls, sessionCalls int
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			sessionCalls++
			if got := r.Header.Get("Cookie"); got != "__Secure-next-auth.session-token=fresh-cookie" {
				t.Fatalf("session cookie = %q, want next-auth cookie", got)
			}
			_, _ = w.Write([]byte(`{"accessToken":"` + newAccess + `","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"},"user":{"email":"user@example.com"}}`))
		case "/backend-api/codex/responses":
			codexCalls++
			auths = append(auths, r.Header.Get("Authorization"))
			if codexCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null}}`))
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+newAccess {
				t.Fatalf("retry authorization = %q, want cookie-recovered token", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := rewriteTransportClient(server.URL)
	p.httpClient = client
	p.resolver.httpClient = client

	rc, status, err := p.InvokeRaw(ctx, acc.ID, []byte(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatalf("InvokeRaw: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		body, _ := io.ReadAll(rc)
		t.Fatalf("status = %d body=%s", status, body)
	}
	if sessionCalls != 1 || codexCalls != 2 {
		t.Fatalf("calls session=%d codex=%d, want 1/2", sessionCalls, codexCalls)
	}
	if len(auths) != 2 || auths[0] != "Bearer "+oldAccess || auths[1] != "Bearer "+newAccess {
		t.Fatalf("authorization sequence = %#v", auths)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatalf("stored access token = %q, want cookie-recovered token", parsed.AccessToken)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" {
		t.Fatalf("refresh token should remain empty when cookie session omits it: session=%q top=%q", parsed.RefreshToken, sec.RefreshToken)
	}
}

func TestInvokeRealRefreshesAndRetriesTokenInvalidated(t *testing.T) {
	ctx := context.Background()
	p := New(ModeReal)
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-token-invalidated-ir",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  oldAccess,
			RefreshToken: "rt-old",
			Expires:      time.Now().Add(time.Hour),
			AccountID:    "chatgpt-account",
			PlanType:     "plus",
			Email:        "user@example.com",
			IDToken:      "id-old",
		}),
		RefreshToken: "rt-old",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+newAccess {
			t.Fatalf("retry authorization = %q, want refreshed token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	defer server.Close()
	p.httpClient = rewriteTransportClient(server.URL)

	ch, err := p.Invoke(ctx, acc, &ir.Request{
		Model: "gpt-5.5",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}},
		}},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var text string
	for ev := range ch {
		switch ev.Kind {
		case ir.EvTextDelta:
			text += ev.Text
		case ir.EvError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "ok" {
		t.Fatalf("stream text = %q, want ok", text)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestBuildResponsesBodyPreservesFastTierAndUsesXHighDefaultEffort(t *testing.T) {
	body, err := buildResponsesBody(&ir.Request{
		Model:       "gpt-5.5",
		ServiceTier: "fast",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}},
		}},
	}, "gpt-5.5")
	if err != nil {
		t.Fatalf("buildResponsesBody: %v", err)
	}
	if got := gjson.GetBytes(body, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, body)
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
