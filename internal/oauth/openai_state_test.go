package oauth

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/store"
)

func TestCodexRelayStateIsHexAndDecodable(t *testing.T) {
	state := encodeRelayState(ProviderCodex, strings.Repeat("a", 64), "https://admin.example.com")
	if len(state) <= 64 {
		t.Fatalf("state did not include relay data: %q", state)
	}
	for _, ch := range state {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Fatalf("codex relay state contains non-hex character %q in %q", ch, state)
		}
	}
	base, ok := RelayBaseFromState(ProviderCodex, state)
	if !ok || base != "https://admin.example.com" {
		t.Fatalf("relay base = %q, %v; want https://admin.example.com, true", base, ok)
	}
}

func TestRelayCallbackURLCopiesCallbackQuery(t *testing.T) {
	state := encodeRelayState(ProviderCodex, strings.Repeat("b", 64), "http://127.0.0.1:8080")
	q := url.Values{}
	q.Set("state", state)
	q.Set("code", "auth-code")

	relayURL, ok := RelayCallbackURL(ProviderCodex, q)
	if !ok {
		t.Fatal("expected relay callback URL")
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		t.Fatalf("parse relay url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "http://127.0.0.1:8080/accounts/oauth/callback/relay" {
		t.Fatalf("relay target = %q", got)
	}
	if u.Query().Get("provider") != string(ProviderCodex) || u.Query().Get("state") != state || u.Query().Get("code") != "auth-code" {
		t.Fatalf("relay query was not preserved: %s", u.RawQuery)
	}
}

func TestCodexStartBuildsCPACompatibleAuthorizeURL(t *testing.T) {
	m := New()
	p, authURL, err := m.StartWithRelayBase(ProviderCodex, "default", "", "https://admin.example.com")
	if err != nil {
		t.Fatalf("start codex oauth: %v", err)
	}
	if p.Provider != ProviderCodex {
		t.Fatalf("provider = %s", p.Provider)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != CodexConfig.AuthorizeURL {
		t.Fatalf("authorize target = %q", got)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"client_id":                  CodexConfig.ClientID,
		"response_type":              "code",
		"redirect_uri":               CodexConfig.RedirectURI,
		"scope":                      "openid email profile offline_access",
		"code_challenge_method":      "S256",
		"prompt":                     "login",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	} {
		if got := q.Get(key); got != want {
			t.Fatalf("%s = %q, want %q; url=%s", key, got, want, authURL)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Fatalf("missing code_challenge: %s", authURL)
	}
	if _, ok := RelayBaseFromState(ProviderCodex, q.Get("state")); !ok {
		t.Fatalf("codex state should carry relay base as hex: %q", q.Get("state"))
	}
}

func TestKiroStartBuildsOIDCAuthorizeURL(t *testing.T) {
	oldRegister := registerKiroOIDCClient
	registerKiroOIDCClient = func(ctx context.Context, region, redirectURI string) (string, string, error) {
		if region != "us-east-1" {
			t.Fatalf("region = %q", region)
		}
		if redirectURI != KiroConfig.RedirectURI {
			t.Fatalf("redirectURI = %q", redirectURI)
		}
		return "kiro-client-id", "kiro-client-secret", nil
	}
	defer func() { registerKiroOIDCClient = oldRegister }()

	m := New()
	p, authURL, err := m.StartWithRelayBase(ProviderKiro, "default", "", "https://admin.example.com")
	if err != nil {
		t.Fatalf("start kiro oauth: %v", err)
	}
	if p.Provider != ProviderKiro {
		t.Fatalf("provider = %s", p.Provider)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != KiroConfig.AuthorizeURL {
		t.Fatalf("authorize target = %q", got)
	}
	q := u.Query()
	if q.Get("client_id") != "kiro-client-id" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != KiroConfig.RedirectURI {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("missing PKCE params: %s", u.RawQuery)
	}
	if q.Get("scopes") != KiroConfig.Scope {
		t.Fatalf("scopes = %q", q.Get("scopes"))
	}
	if _, ok := RelayBaseFromState(ProviderKiro, q.Get("state")); !ok {
		t.Fatalf("kiro state should carry relay base: %q", q.Get("state"))
	}
	if p.OAuthClientID != "kiro-client-id" || p.OAuthClientSecret != "kiro-client-secret" || p.OAuthRegion != "us-east-1" {
		t.Fatalf("kiro client fields not saved on pending auth: %+v", p)
	}
}

func TestKiroPendingAuthReloadPreservesOIDCClientFields(t *testing.T) {
	oldRegister := registerKiroOIDCClient
	registerKiroOIDCClient = func(ctx context.Context, region, redirectURI string) (string, string, error) {
		return "reload-client-id", "reload-client-secret", nil
	}
	defer func() { registerKiroOIDCClient = oldRegister }()

	st, err := store.Open(filepath.Join(t.TempDir(), "oauth.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	creator := New()
	creator.SetStore(st)
	p, _, err := creator.StartWithRelayBase(ProviderKiro, "default", "note", "https://admin.example.com")
	if err != nil {
		t.Fatalf("start kiro oauth: %v", err)
	}

	reloaded := New()
	reloaded.SetStore(st)
	got, ok := reloaded.FindByState(p.State)
	if !ok {
		t.Fatal("pending auth was not reloaded from store")
	}
	if got.OAuthClientID != "reload-client-id" || got.OAuthClientSecret != "reload-client-secret" || got.OAuthRegion != "us-east-1" {
		t.Fatalf("oidc client fields were not restored: %+v", got)
	}
	if got.CodeVerifier == "" || got.RedirectURI != KiroConfig.RedirectURI {
		t.Fatalf("pkce/redirect fields were not restored: %+v", got)
	}
}

func TestParseKiroTokenAcceptsSnakeCaseAndBuildsSession(t *testing.T) {
	m := New()
	p := &PendingAuth{
		ID:                "pending-kiro",
		Provider:          ProviderKiro,
		OAuthClientID:     "client-id",
		OAuthClientSecret: "client-secret",
		OAuthRegion:       "us-east-1",
	}

	err := m.parseKiroToken(p, []byte(`{
		"access_token":"access-1",
		"refresh_token":"refresh-1",
		"profile_arn":"profile-1",
		"expires_in":1800
	}`))
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if p.AccessToken != "access-1" || p.RefreshToken != "refresh-1" || p.ProfileArn != "profile-1" || p.PlanType != "kiro" {
		t.Fatalf("unexpected parsed pending auth: %+v", p)
	}

	var session map[string]string
	if err := json.Unmarshal([]byte(m.BuildSessionJSON(p)), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if session["profileArn"] != "profile-1" || session["client_id"] != "client-id" || session["client_secret"] != "client-secret" {
		t.Fatalf("kiro session missing oidc fields: %#v", session)
	}
}
