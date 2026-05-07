package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
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

func testJWTExp(exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
