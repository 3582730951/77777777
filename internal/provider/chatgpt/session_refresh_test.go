package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

func TestRefreshCredentialPersistsRefreshedSession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	oldSession := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  oldAccess,
		RefreshToken: "rt-old",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-old",
	})
	acc := &domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: oldSession,
		RefreshToken: "rt-old",
		Cookies:      []byte(`{"keep":true}`),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
	if string(sec.Cookies) != `{"keep":true}` {
		t.Fatalf("cookies were not preserved: %s", string(sec.Cookies))
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatalf("stored access token was not refreshed")
	}
	if parsed.RefreshToken != "rt-new" {
		t.Fatalf("stored session refresh token = %q, want rt-new", parsed.RefreshToken)
	}
	if parsed.Email != "user@example.com" {
		t.Fatalf("stored email = %q, want user@example.com", parsed.Email)
	}
	if acc.Email != "user@example.com" {
		t.Fatalf("account email = %q, want user@example.com", acc.Email)
	}
}

func TestRefreshCredentialPrefersSessionRefreshTokenOverStaleSecret(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	sessionWithCurrentRefresh := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-current",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-old",
	})
	acc := &domain.Account{
		ID:       "acc-stale-secret",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: sessionWithCurrentRefresh,
		RefreshToken: "rt-stale",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-current" {
			t.Fatalf("refresh token = %q, want rt-current", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
}

func TestRefreshCredentialUsesOtherCodexAuthJSONRefreshToken(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	otherCodexAuthJSON := `{"auth_mode":"chatgpt","tokens":{"access_token":"` + expiredAccess + `","refresh_token":"rt-authjson","id_token":"id-old","account_id":"chatgpt-account"},"last_refresh":"2026-01-01T00:00:00Z"}`
	acc := &domain.Account{
		ID:       "acc-other-codex-auth-json",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: otherCodexAuthJSON,
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-authjson" {
			t.Fatalf("refresh token = %q, want rt-authjson", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored top-level refresh token = %q, want rt-new", sec.RefreshToken)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != newAccess || parsed.RefreshToken != "rt-new" || parsed.AccountID != "chatgpt-account" {
		t.Fatalf("persisted session did not use refreshed other_codex credentials: %+v", parsed)
	}
}

func TestParseSessionJSONExtractsOtherCodexAccountFeatures(t *testing.T) {
	idToken := testJWTClaims(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         "acct-from-claim",
			"chatgpt_plan_type":          "team",
			"chatgpt_user_id":            "user-from-claim",
			"chatgpt_account_is_fedramp": true,
		},
	})
	sessionJSON := `{"auth_mode":"chatgpt","user":{"id":"acct-from-claim"},"tokens":{"access_token":"` + testJWTExp(time.Now().Add(time.Hour)) + `","refresh_token":"rt","id_token":"` + idToken + `"}}`

	parsed, err := parseSessionJSON([]byte(sessionJSON))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if parsed.AccountID != "acct-from-claim" {
		t.Fatalf("account id = %q, want acct-from-claim", parsed.AccountID)
	}
	if parsed.PlanType != "team" {
		t.Fatalf("plan type = %q, want team", parsed.PlanType)
	}
	if parsed.Email != "user@example.com" {
		t.Fatalf("email = %q, want user@example.com", parsed.Email)
	}
	if parsed.ChatGPTUserID != "user-from-claim" {
		t.Fatalf("chatgpt user id = %q, want user-from-claim", parsed.ChatGPTUserID)
	}
	if !parsed.FedRAMP {
		t.Fatal("fedramp flag was not extracted")
	}
}

func TestNormalizeSessionOnlyAuthJSONStripsRefreshTokenAndMatchesCodexShape(t *testing.T) {
	idToken := testJWTClaims(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "session@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         "acct-session",
			"chatgpt_plan_type":          "plus",
			"chatgpt_user_id":            "user-session",
			"chatgpt_account_is_fedramp": true,
		},
	})
	accessToken := testJWTClaims(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	rawSession := `{"accessToken":"` + accessToken + `","refreshToken":"rt-should-not-survive","idToken":"` + idToken + `","expires":"2099-01-01T00:00:00Z","account":{"id":"acct-input","planType":"team"},"user":{"id":"user-input","email":"session@example.com"}}`

	normalized, err := NormalizeSessionOnlyAuthJSON(rawSession)
	if err != nil {
		t.Fatalf("normalize session-only auth json: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(normalized), &raw); err != nil {
		t.Fatalf("unmarshal normalized json: %v", err)
	}
	if raw["auth_mode"] != "chatgpt" {
		t.Fatalf("auth_mode = %v, want chatgpt", raw["auth_mode"])
	}
	if raw["refreshToken"] != "" || raw["refresh_token"] != "" {
		t.Fatalf("top-level refresh token survived: refreshToken=%v refresh_token=%v", raw["refreshToken"], raw["refresh_token"])
	}
	if _, ok := raw["last_refresh"].(string); !ok {
		t.Fatalf("last_refresh missing or not string: %v", raw["last_refresh"])
	}
	tokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens missing: %#v", raw["tokens"])
	}
	if tokens["access_token"] != accessToken || tokens["id_token"] != idToken {
		t.Fatalf("tokens did not preserve access/id token: %#v", tokens)
	}
	if tokens["refresh_token"] != "" {
		t.Fatalf("tokens.refresh_token = %v, want empty", tokens["refresh_token"])
	}
	if tokens["account_id"] != "acct-input" || tokens["chatgpt_plan_type"] != "team" {
		t.Fatalf("tokens account features = %#v", tokens)
	}

	parsed, err := parseSessionJSON([]byte(normalized))
	if err != nil {
		t.Fatalf("parse normalized session: %v", err)
	}
	if parsed.RefreshToken != "" {
		t.Fatalf("parsed refresh token = %q, want empty", parsed.RefreshToken)
	}
	if parsed.Email != "session@example.com" || parsed.AccountID != "acct-input" || parsed.PlanType != "team" {
		t.Fatalf("parsed features not preserved: %+v", parsed)
	}
	if !parsed.FedRAMP {
		t.Fatal("fedramp flag was not preserved")
	}
}

func TestNormalizeSessionOnlyAuthJSONBuildsSyntheticIDTokenForCPA(t *testing.T) {
	accessToken := testJWTClaims(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	rawSession := `{"accessToken":"` + accessToken + `","expires":"2099-01-01T00:00:00Z","account":{"id":"acct-cpa","planType":"plus"},"user":{"id":"user-cpa","email":"cpa@example.com"}}`

	normalized, err := NormalizeSessionOnlyAuthJSON(rawSession)
	if err != nil {
		t.Fatalf("normalize session-only auth json: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(normalized), &raw); err != nil {
		t.Fatalf("unmarshal normalized json: %v", err)
	}
	idToken, _ := raw["id_token"].(string)
	if idToken == "" || idToken == accessToken {
		t.Fatalf("id_token = %q, want synthetic token distinct from access token", idToken)
	}
	if raw["id_token_synthetic"] != true {
		t.Fatalf("id_token_synthetic = %v, want true", raw["id_token_synthetic"])
	}
	claims := codexClaims(idToken)
	if claims.AccountID != "acct-cpa" || claims.PlanType != "plus" || claims.UserID != "user-cpa" || claims.Email != "cpa@example.com" {
		t.Fatalf("synthetic id_token claims not CPA-compatible: %+v", claims)
	}
	parsed, err := parseSessionJSON([]byte(normalized))
	if err != nil {
		t.Fatalf("parse normalized session: %v", err)
	}
	if parsed.IDToken != idToken || parsed.RefreshToken != "" || parsed.AccountID != "acct-cpa" || parsed.PlanType != "plus" {
		t.Fatalf("parsed synthetic session mismatch: %+v", parsed)
	}
}

func TestRefreshCredentialDoesNotRefreshSessionOnlyAuthJSON(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	sessionOnly, err := NormalizeSessionOnlyAuthJSON(`{"accessToken":"` + expiredAccess + `","refreshToken":"rt-should-not-be-used","expires":"2000-01-01T00:00:00Z","account":{"id":"acct-session","planType":"plus"},"user":{"email":"session@example.com"}}`)
	if err != nil {
		t.Fatalf("normalize session-only auth json: %v", err)
	}
	acc := &domain.Account{
		ID:       "acc-session-only-json",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{SessionToken: sessionOnly}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var calls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		calls.Add(1)
		return testJWTExp(time.Now().Add(time.Hour)), "rt-new", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err == nil {
		t.Fatal("refresh credential succeeded with expired session-only token")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("refresh callback calls = %d, want 0", got)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" || len(sec.Cookies) != 0 {
		t.Fatalf("session-only credentials gained refresh material: parsed_rt=%q top_rt=%q cookies=%q", parsed.RefreshToken, sec.RefreshToken, string(sec.Cookies))
	}
}

func TestRefreshCredentialTreatsTaggedSessionOnlyAsNonRecoverableSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	sessionOnly, err := NormalizeSessionOnlyAuthJSON(`{"accessToken":"` + expiredAccess + `","expires":"2000-01-01T00:00:00Z","account":{"id":"acct-session","planType":"plus"},"user":{"email":"session@example.com"}}`)
	if err != nil {
		t.Fatalf("normalize session-only auth json: %v", err)
	}
	acc := &domain.Account{
		ID:       "acc-session-only-tagged",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: sessionOnly,
		RefreshToken: "rt-must-not-be-used",
		Cookies:      []byte("__Secure-next-auth.session-token=must-not-be-used"),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var refreshCalls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		refreshCalls.Add(1)
		return testJWTExp(time.Now().Add(time.Hour)), "rt-new", "id-new", 3600, nil
	})
	var sessionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/session" {
			sessionCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := rewriteTransportClient(server.URL)
	p.httpClient = client
	p.resolver.httpClient = client

	if err := p.RefreshCredential(ctx, acc); err == nil {
		t.Fatal("refresh credential succeeded with expired tagged session-only token")
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh callback calls = %d, want 0", got)
	}
	if got := sessionCalls.Load(); got != 0 {
		t.Fatalf("auth/session calls = %d, want 0", got)
	}
}

func TestBuildChatGPTSessionJSONPreservesAccountFeatures(t *testing.T) {
	sessionJSON := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:   testJWTExp(time.Now().Add(time.Hour)),
		RefreshToken:  "rt",
		Expires:       time.Now().Add(time.Hour),
		AccountID:     "acct-1",
		PlanType:      "team",
		Email:         "user@example.com",
		IDToken:       "id-token",
		ChatGPTUserID: "user-1",
		FedRAMP:       true,
	})

	parsed, err := parseSessionJSON([]byte(sessionJSON))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if parsed.AccountID != "acct-1" || parsed.ChatGPTUserID != "user-1" || !parsed.FedRAMP {
		t.Fatalf("account features not preserved: %+v", parsed)
	}
}

func TestRefreshCredentialPreservesRefreshTokenWhenAuthorityOmitsRotation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	acc := &domain.Account{
		ID:       "acc-empty-refresh-rotation",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken: expiredAccess,
			Expires:     time.Now().Add(-time.Minute),
			AccountID:   "chatgpt-account",
			PlanType:    "plus",
			Email:       "user@example.com",
			IDToken:     "id-old",
		}),
		RefreshToken: "rt-old",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		return newAccess, "", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatalf("stored access token was not refreshed")
	}
	if parsed.RefreshToken != "rt-old" || sec.RefreshToken != "rt-old" {
		t.Fatalf("missing rotated refresh token should preserve existing refresh token: session=%q top=%q", parsed.RefreshToken, sec.RefreshToken)
	}
}

func TestRefreshCredentialRefreshesWebSessionFromStoredCookie(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(10 * time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	acc := &domain.Account{
		ID:       "acc-web-session-refresh",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		UA:       "codex-cli-test",
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken: oldAccess,
			Expires:     time.Now().Add(10 * time.Minute),
			AccountID:   "chatgpt-account",
			PlanType:    "plus",
			Email:       "user@example.com",
			IDToken:     "id-old",
		}),
		Cookies: []byte(`__Secure-next-auth.session-token=old-cookie; other=keep`),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	var sessionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/session" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		sessionCalls++
		cookie := r.Header.Get("Cookie")
		if !strings.Contains(cookie, "__Secure-next-auth.session-token=old-cookie") || !strings.Contains(cookie, "other=keep") {
			t.Fatalf("cookie header = %q, want stored web cookies", cookie)
		}
		w.Header().Add("Set-Cookie", "__Secure-next-auth.session-token=new-cookie; Path=/; HttpOnly; Secure")
		_, _ = w.Write([]byte(`{"accessToken":"` + newAccess + `","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"},"user":{"email":"user@example.com"}}`))
	}))
	defer server.Close()

	p := New(ModeReal)
	p.SetStore(st)
	client := rewriteTransportClient(server.URL)
	p.httpClient = client
	p.resolver.httpClient = client

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}
	if sessionCalls != 1 {
		t.Fatalf("session calls = %d, want 1", sessionCalls)
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
		t.Fatalf("stored access token = %q, want refreshed web session token", parsed.AccessToken)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" {
		t.Fatalf("web session refresh should not invent refresh token: session=%q top=%q", parsed.RefreshToken, sec.RefreshToken)
	}
	cookies := string(sec.Cookies)
	if !strings.Contains(cookies, "__Secure-next-auth.session-token=new-cookie") || !strings.Contains(cookies, "other=keep") {
		t.Fatalf("cookies were not merged with Set-Cookie: %q", cookies)
	}
}

func TestChatGPTCookieHeaderFromSecretParsesDevToolsCookieHeader(t *testing.T) {
	sec := store.AccountSecret{Cookies: []byte(strings.Join([]string{
		"Host: chatgpt.com",
		`sec-ch-ua: "Chromium";v="124"`,
		"Cookie: __Secure-next-auth.session-token.0=part0; __Secure-next-auth.session-token.1=part1; other=keep",
		"Accept: */*",
	}, "\n"))}

	got := chatGPTCookieHeaderFromSecret(sec)
	want := "__Secure-next-auth.session-token.0=part0; __Secure-next-auth.session-token.1=part1; other=keep"
	if got != want {
		t.Fatalf("cookie header = %q, want %q", got, want)
	}
	if cookie := chatGPTNextAuthSessionCookie(sec); cookie != "part0part1" {
		t.Fatalf("next-auth session cookie = %q, want concatenated chunks", cookie)
	}
}

func TestChatGPTCookieHeaderFromSecretParsesChromeApplicationCookieTable(t *testing.T) {
	sec := store.AccountSecret{Cookies: []byte(strings.Join([]string{
		"Name\tValue\tDomain\tPath\tExpires\tSize\tHttpOnly\tSecure\tSameSite\tPriority",
		"__Secure-next-auth.session-token.0\tpart0\t.chatgpt.com\t/\t2026-08-24T05:54:55.225Z\t3967\t✓\t✓\tLax\tMedium",
		"__Secure-next-auth.session-token.1\tpart1\t.chatgpt.com\t/\t2026-08-24T05:54:55.227Z\t77\t✓\t✓\tLax\tMedium",
		"cf_clearance\tclear-token\t.chatgpt.com\t/\t2027-05-26T05:51:27.062Z\t417\t✓\t✓\tNone\tMedium",
	}, "\n"))}

	got := chatGPTCookieHeaderFromSecret(sec)
	for _, want := range []string{
		"__Secure-next-auth.session-token.0=part0",
		"__Secure-next-auth.session-token.1=part1",
		"cf_clearance=clear-token",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cookie header = %q, missing %q", got, want)
		}
	}
	if cookie := chatGPTNextAuthSessionCookie(sec); cookie != "part0part1" {
		t.Fatalf("next-auth session cookie = %q, want concatenated chunks", cookie)
	}
}

func TestForceRefreshNoRefreshTokenReturnsCookieRecoveryError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	acc := &domain.Account{
		ID:       "acc-no-refresh-cookie-fails",
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
		Cookies: []byte("Cookie: __Secure-next-auth.session-token=bad-cookie"),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	originalSecret, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get original secret: %v", err)
	}

	var sessionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/session" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		sessionCalls++
		if got := r.Header.Get("Cookie"); got != "__Secure-next-auth.session-token=bad-cookie" {
			t.Fatalf("session cookie = %q, want parsed Cookie header", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"session expired"}`))
	}))
	defer server.Close()

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		t.Fatalf("refresh callback should not be called without refresh token; got %q", refreshToken)
		return "", "", "", 0, nil
	})
	client := rewriteTransportClient(server.URL)
	p.httpClient = client
	p.resolver.httpClient = client

	_, err = p.forceRefreshSession(ctx, acc, originalSecret)
	if err == nil {
		t.Fatal("force refresh succeeded, want cookie recovery error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stored web session cookie recovery failed") || !strings.Contains(msg, "auth/session 401") {
		t.Fatalf("error = %q, want cookie recovery auth/session failure", msg)
	}
	if strings.Contains(msg, "token invalidated and no refresh_token available") {
		t.Fatalf("error should not return the raw ForceRefresh no-refresh message: %q", msg)
	}
	if sessionCalls != 1 {
		t.Fatalf("session calls = %d, want 1", sessionCalls)
	}
}

func TestTokenInvalidatedDetectionIgnoresTransientChallenge(t *testing.T) {
	challengeBody := []byte(`<html><title>Just a moment...</title>Cloudflare upstream_challenge_blocked token_invalidated</html>`)
	if isChatGPTTokenInvalidatedResponse(401, challengeBody) {
		t.Fatal("challenge response must not trigger refresh_token rotation")
	}
	if isChatGPTRecoverableAuthResponse(401, challengeBody) {
		t.Fatal("challenge response must not trigger auth recovery")
	}
	heavyLoadBody := []byte(`ChatGPT is under heavy load; token_invalidated`)
	if isChatGPTTokenInvalidatedResponse(401, heavyLoadBody) {
		t.Fatal("heavy-load response must not trigger refresh_token rotation")
	}
	if isChatGPTRecoverableAuthResponse(401, heavyLoadBody) {
		t.Fatal("heavy-load response must not trigger auth recovery")
	}
	authBody := []byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`)
	if !isChatGPTTokenInvalidatedResponse(401, authBody) {
		t.Fatal("real token invalidation should still trigger refresh")
	}
	unauthorizedBody := []byte(`{"detail":"Unauthorized"}`)
	if !isChatGPTRecoverableAuthResponse(401, unauthorizedBody) {
		t.Fatal("plain upstream Unauthorized should trigger web-session recovery")
	}
}

func TestRefreshCredentialFallsBackToTopLevelRefreshTokenWhenSessionTokenWasReused(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	sessionWithUsedRefresh := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-used",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-old",
	})
	acc := &domain.Account{
		ID:       "acc-top-level-fallback",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: sessionWithUsedRefresh,
		RefreshToken: "rt-current",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var gotTokens []string
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		gotTokens = append(gotTokens, refreshToken)
		switch refreshToken {
		case "rt-used":
			return "", "", "", 0, errors.New("refresh 401: refresh token has already been used to generate a new access token")
		case "rt-current":
			return newAccess, "rt-new", "id-new", 3600, nil
		default:
			t.Fatalf("unexpected refresh token %q", refreshToken)
			return "", "", "", 0, nil
		}
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}
	if len(gotTokens) != 2 || gotTokens[0] != "rt-used" || gotTokens[1] != "rt-current" {
		t.Fatalf("refresh token attempts = %#v, want [rt-used rt-current]", gotTokens)
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
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != newAccess || parsed.RefreshToken != "rt-new" {
		t.Fatalf("persisted session did not use refreshed credentials: %+v", parsed)
	}
}

func TestRefreshCredentialSerializesConcurrentRefresh(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	oldSession := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-old",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-old",
	})
	acc := &domain.Account{
		ID:       "acc-concurrent",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: oldSession,
		RefreshToken: "rt-old",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var calls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		calls.Add(1)
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		time.Sleep(50 * time.Millisecond)
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- p.RefreshCredential(ctx, acc)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh credential: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
}

func TestForceRefreshSharesCredentialRefreshLock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	oldSession := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-old",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-old",
	})
	acc := &domain.Account{
		ID:       "acc-force-refresh-lock",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: oldSession,
		RefreshToken: "rt-old",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	originalSecret, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get original secret: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var calls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		calls.Add(1)
		if refreshToken != "rt-old" {
			t.Fatalf("refresh token = %q, want rt-old", refreshToken)
		}
		time.Sleep(50 * time.Millisecond)
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- p.RefreshCredential(ctx, acc)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := p.forceRefreshSession(ctx, acc, originalSecret)
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh path returned error: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
}

func TestRefreshCredentialUsesStoredRotatedSessionOverStaleCache(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	staleAccess := testJWTExp(time.Now().Add(30 * time.Minute))
	currentAccess := testJWTExp(time.Now().Add(time.Hour))
	currentSession := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  currentAccess,
		RefreshToken: "rt-current",
		Expires:      time.Now().Add(time.Hour),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-current",
	})
	acc := &domain.Account{
		ID:       "acc-stale-cache-valid",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: currentSession,
		RefreshToken: "rt-current",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.resolver.cache.Store(acc.ID, sessionInfo{
		AccessToken:  staleAccess,
		RefreshToken: "rt-used",
		Expires:      time.Now().Add(30 * time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-stale",
	})
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		t.Fatalf("refresh should not be called for a valid stored session; got %q", refreshToken)
		return "", "", "", 0, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-current" {
		t.Fatalf("stored refresh token = %q, want rt-current", sec.RefreshToken)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != currentAccess {
		t.Fatalf("stored access token was overwritten from stale cache")
	}
	if parsed.RefreshToken != "rt-current" {
		t.Fatalf("stored session refresh token = %q, want rt-current", parsed.RefreshToken)
	}
}

func TestRefreshCredentialSkipsStaleCachedRefreshToken(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	newAccess := testJWTExp(time.Now().Add(time.Hour))
	currentSession := buildChatGPTSessionJSON(sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-current",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-current",
	})
	acc := &domain.Account{
		ID:       "acc-stale-cache-expired",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: currentSession,
		RefreshToken: "rt-current",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.resolver.cache.Store(acc.ID, sessionInfo{
		AccessToken:  expiredAccess,
		RefreshToken: "rt-used",
		Expires:      time.Now().Add(-time.Minute),
		AccountID:    "chatgpt-account",
		PlanType:     "plus",
		Email:        "user@example.com",
		IDToken:      "id-stale",
	})
	var calls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		calls.Add(1)
		if refreshToken != "rt-current" {
			t.Fatalf("refresh token = %q, want rt-current", refreshToken)
		}
		return newAccess, "rt-new", "id-new", 3600, nil
	})

	if err := p.RefreshCredential(ctx, acc); err != nil {
		t.Fatalf("refresh credential: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.RefreshToken != "rt-new" {
		t.Fatalf("stored refresh token = %q, want rt-new", sec.RefreshToken)
	}
}

func TestRefreshCredentialClearsReusedRefreshTokenWhenNoRecovery(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	expiredAccess := testJWTExp(time.Now().Add(-time.Minute))
	acc := &domain.Account{
		ID:       "acc-reused-clear",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
		SessionToken: buildChatGPTSessionJSON(sessionInfo{
			AccessToken:  expiredAccess,
			RefreshToken: "rt-used",
			Expires:      time.Now().Add(-time.Minute),
			AccountID:    "chatgpt-account",
			PlanType:     "plus",
			Email:        "user@example.com",
			IDToken:      "id-old",
		}),
		RefreshToken: "rt-used",
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		if refreshToken != "rt-used" {
			t.Fatalf("refresh token = %q, want rt-used", refreshToken)
		}
		return "", "", "", 0, errors.New("refresh 401: refresh_token_reused")
	})

	if err := p.RefreshCredential(ctx, acc); err == nil {
		t.Fatal("refresh credential succeeded, want refresh_token_reused error")
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" {
		t.Fatalf("reused refresh token should be cleared: session=%q top=%q", parsed.RefreshToken, sec.RefreshToken)
	}
}

func TestForceRefreshRecoversStoreSessionChangedAfterRefreshReuse(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	oldAccess := testJWTExp(time.Now().Add(time.Hour))
	newAccess := testJWTExp(time.Now().Add(2 * time.Hour))
	acc := &domain.Account{
		ID:       "acc-reused-race-cleared",
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
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
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	originalSecret, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get original secret: %v", err)
	}

	p := New(ModeReal)
	p.SetStore(st)
	var calls atomic.Int32
	p.SetRefreshFunc(func(ctx context.Context, refreshToken string) (string, string, string, int, error) {
		calls.Add(1)
		if refreshToken != "rt-used" {
			t.Fatalf("refresh token = %q, want rt-used", refreshToken)
		}
		if err := st.UpsertAccount(ctx, acc, store.AccountSecret{
			SessionToken: buildChatGPTSessionJSON(sessionInfo{
				AccessToken:  newAccess,
				RefreshToken: "",
				Expires:      time.Now().Add(2 * time.Hour),
				AccountID:    "chatgpt-account",
				PlanType:     "plus",
				Email:        "user@example.com",
				IDToken:      "id-new",
			}),
			RefreshToken: "",
		}); err != nil {
			t.Fatalf("persist concurrent session: %v", err)
		}
		return "", "", "", 0, errors.New("refresh 401: refresh_token_reused")
	})

	info, err := p.forceRefreshSession(ctx, acc, originalSecret)
	if err != nil {
		t.Fatalf("force refresh should recover changed stored session: %v", err)
	}
	if info.AccessToken != newAccess {
		t.Fatalf("recovered access token = %q, want new token", info.AccessToken)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	parsed, err := parseSessionJSON([]byte(sec.SessionToken))
	if err != nil {
		t.Fatalf("parse persisted session: %v", err)
	}
	if parsed.AccessToken != newAccess {
		t.Fatalf("stored access token = %q, want new token", parsed.AccessToken)
	}
	if parsed.RefreshToken != "" || sec.RefreshToken != "" {
		t.Fatalf("stored refresh token = session %q top %q, want both empty", parsed.RefreshToken, sec.RefreshToken)
	}
}

func testJWTExp(exp time.Time) string {
	return testJWTClaims(map[string]any{"exp": exp.Unix()})
}

func testJWTClaims(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
