package admin

import (
	"bytes"
	"html/template"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthShowAuthURLKeepsInnerOAuthEncoding(t *testing.T) {
	authURL := "https://auth.openai.com/oauth/authorize?" +
		"response_type=code" +
		"&client_id=app_EMoamEEZ73f0CkXaXp7hrann" +
		"&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback" +
		"&scope=openid%20profile%20email%20offline_access%20api.connectors.read%20api.connectors.invoke" +
		"&code_challenge=challenge" +
		"&code_challenge_method=S256" +
		"&id_token_add_organizations=true" +
		"&codex_cli_simplified_flow=true" +
		"&state=state" +
		"&originator=codex_cli_rs"

	req := httptest.NewRequest("GET", "/accounts/oauth/pending?u="+url.QueryEscape(authURL), nil)
	got := oauthShowAuthURL(req)
	if got != authURL {
		t.Fatalf("oauth show URL decoded inner query:\n got: %s\nwant: %s", got, authURL)
	}
	if strings.Contains(got, "scope=openid profile") || strings.Contains(got, "redirect_uri=http://") {
		t.Fatalf("inner OAuth query must remain percent-encoded, got: %s", got)
	}
}

func TestOAuthAuthURLTemplatePreservesPercentEscapes(t *testing.T) {
	authURL := "https://auth.openai.com/oauth/authorize?" +
		"redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback" +
		"&scope=openid%20profile%20email%20offline_access" +
		"&state=state"

	tpl := template.Must(template.New("oauth-link").Parse(`<a href="{{.}}">{{.}}</a>`))
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, authURL); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback") {
		t.Fatalf("template lost redirect_uri percent escapes: %s", out)
	}
	if !strings.Contains(out, "scope=openid%20profile%20email%20offline_access") {
		t.Fatalf("template lost scope percent escapes: %s", out)
	}
	if strings.Contains(out, "redirect_uri=http://") || strings.Contains(out, "%253A%252F%252F") {
		t.Fatalf("template decoded or double-encoded inner URL: %s", out)
	}
}
