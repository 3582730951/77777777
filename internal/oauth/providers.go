// Package oauth implements the OAuth flows for the subscription providers our
// pool supports: OpenAI Codex, Anthropic Claude, Google Gemini, and Kiro.
//
// Each provider has its own:
//   - ClientID + ClientSecret (where applicable)
//   - Authorize / Token URLs
//   - Callback port (1455 / 54545 / 8085 — matches the official CLIs so the
//     same redirect_uri is pre-registered)
//   - Scope list
//   - Extra request quirks (e.g. Codex's `codex_cli_simplified_flow=true`)
//
// Constants are extracted from the official CLI binaries / open-source
// reference implementations (CPA / sub2api). Don't change them blindly — the
// provider rejects unknown client_ids.
package oauth

import "os"

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderGemini Provider = "gemini"
	ProviderKiro   Provider = "kiro"
)

// ProviderConfig is the static config per provider.
type ProviderConfig struct {
	ID           Provider
	DisplayName  string
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	ClientSecret string // empty for public PKCE clients
	RedirectURI  string
	CallbackPort int
	CallbackPath string
	Scope        string
	ExtraParams  map[string]string
}

const (
	// CodexDefaultOriginator is the wire identity used by Codex CLI-compatible
	// OAuth and upstream requests.
	CodexDefaultOriginator = "codex_cli_rs"

	// CodexLoginScope mirrors current other_codex login. The connector scopes
	// are required by the ChatGPT Codex backend; refresh follows the official
	// JSON refresh_token exchange and does not resend authorize scope.
	CodexLoginScope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// Codex / OpenAI — constants from sub2api plus current Codex Manager OAuth
// authorize parameters.
var CodexConfig = ProviderConfig{
	ID:           ProviderCodex,
	DisplayName:  "OpenAI Codex",
	AuthorizeURL: "https://auth.openai.com/oauth/authorize",
	TokenURL:     "https://auth.openai.com/oauth/token",
	ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
	RedirectURI:  "http://localhost:1455/auth/callback",
	CallbackPort: 1455,
	CallbackPath: "/auth/callback",
	Scope:        CodexLoginScope,
	ExtraParams: map[string]string{
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 CodexDefaultOriginator,
	},
}

// Claude / Anthropic — mirrored from sub2api/internal/pkg/oauth/oauth.go.
// TokenURL and RedirectURI must match platform.claude.com, NOT api.anthropic.com.
// The browser authorize URL prepends "org:create_api_key" to scope.
var ClaudeConfig = ProviderConfig{
	ID:           ProviderClaude,
	DisplayName:  "Anthropic Claude",
	AuthorizeURL: "https://claude.ai/oauth/authorize",
	TokenURL:     "https://platform.claude.com/v1/oauth/token",
	ClientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
	RedirectURI:  "https://platform.claude.com/oauth/code/callback",
	CallbackPort: 54545, // kept for display; actual callback is handled by platform.claude.com
	CallbackPath: "/oauth/code/callback",
	// ScopeOAuth — includes org:create_api_key for the browser authorize URL
	Scope: "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
	ExtraParams: map[string]string{
		"code": "true",
	},
}

// Gemini / Google. Provide these with GEMINI_OAUTH_CLIENT_ID and
// GEMINI_OAUTH_CLIENT_SECRET; GitHub push protection rejects checked-in OAuth
// client credentials even when they come from public reference implementations.
var GeminiConfig = ProviderConfig{
	ID:           ProviderGemini,
	DisplayName:  "Google Gemini",
	AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:     "https://oauth2.googleapis.com/token",
	ClientID:     firstEnv("GEMINI_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_ID"),
	ClientSecret: firstEnv("GEMINI_OAUTH_CLIENT_SECRET", "GOOGLE_OAUTH_CLIENT_SECRET"),
	RedirectURI:  "http://localhost:8085/oauth2callback",
	CallbackPort: 8085,
	CallbackPath: "/oauth2callback",
	Scope:        "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile",
	ExtraParams: map[string]string{
		"access_type":            "offline",
		"prompt":                 "consent",
		"include_granted_scopes": "true",
	},
}

// Kiro uses AWS IAM Identity Center OIDC with PKCE. We register a short-lived
// public client at flow start, then use its clientId/clientSecret for exchange
// and later refreshes.
var KiroConfig = ProviderConfig{
	ID:           ProviderKiro,
	DisplayName:  "Kiro",
	AuthorizeURL: "https://oidc.us-east-1.amazonaws.com/authorize",
	TokenURL:     "https://oidc.us-east-1.amazonaws.com/token",
	RedirectURI:  "http://127.0.0.1:19876/oauth/callback",
	CallbackPort: 19876,
	CallbackPath: "/oauth/callback",
	Scope:        "codewhisperer:completions codewhisperer:analysis codewhisperer:conversations codewhisperer:transformations codewhisperer:taskassist",
	ExtraParams: map[string]string{
		"issuer_url": "https://view.awsapps.com/start",
		"region":     "us-east-1",
	},
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

// ConfigFor returns the provider config or nil if unknown.
func ConfigFor(p Provider) *ProviderConfig {
	switch p {
	case ProviderCodex:
		return &CodexConfig
	case ProviderClaude:
		return &ClaudeConfig
	case ProviderGemini:
		return &GeminiConfig
	case ProviderKiro:
		return &KiroConfig
	}
	return nil
}

// AllProviders returns every supported provider for UI dropdowns.
func AllProviders() []ProviderConfig {
	return []ProviderConfig{CodexConfig, ClaudeConfig, GeminiConfig, KiroConfig}
}
