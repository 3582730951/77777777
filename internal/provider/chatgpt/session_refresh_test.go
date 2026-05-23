package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
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

func TestTokenInvalidatedDetectionIgnoresTransientChallenge(t *testing.T) {
	challengeBody := []byte(`<html><title>Just a moment...</title>Cloudflare upstream_challenge_blocked token_invalidated</html>`)
	if isChatGPTTokenInvalidatedResponse(401, challengeBody) {
		t.Fatal("challenge response must not trigger refresh_token rotation")
	}
	heavyLoadBody := []byte(`ChatGPT is under heavy load; token_invalidated`)
	if isChatGPTTokenInvalidatedResponse(401, heavyLoadBody) {
		t.Fatal("heavy-load response must not trigger refresh_token rotation")
	}
	authBody := []byte(`{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`)
	if !isChatGPTTokenInvalidatedResponse(401, authBody) {
		t.Fatal("real token invalidation should still trigger refresh")
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

func testJWTExp(exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
