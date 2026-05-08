package oauth

import (
	"net/url"
	"strings"
	"testing"
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

func TestKiroStartBuildsDesktopAuthURL(t *testing.T) {
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
	if q.Get("idp") != "BuilderId" {
		t.Fatalf("idp = %q", q.Get("idp"))
	}
	if q.Get("redirectUri") != KiroConfig.RedirectURI {
		t.Fatalf("redirectUri = %q", q.Get("redirectUri"))
	}
	if q.Get("codeChallenge") == "" || q.Get("codeChallengeMethod") != "S256" {
		t.Fatalf("missing PKCE params: %s", u.RawQuery)
	}
	if q.Get("client_id") != "" {
		t.Fatalf("kiro desktop auth must not include client_id: %s", u.RawQuery)
	}
	if _, ok := RelayBaseFromState(ProviderKiro, q.Get("state")); !ok {
		t.Fatalf("kiro state should carry relay base: %q", q.Get("state"))
	}
}
