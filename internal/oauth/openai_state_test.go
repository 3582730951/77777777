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
