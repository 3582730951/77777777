// openai.go (renamed conceptually to manager.go) — generic PKCE OAuth flow
// driver. Works for any of the three providers defined in providers.go.
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PendingAuth tracks one OAuth flow in progress.
type PendingAuth struct {
	ID           string
	Provider     Provider
	State        string
	CodeVerifier string
	RedirectURI  string
	TenantID     string
	Note         string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Status       string // pending / success / failed
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Email        string
	PlanType     string
	Organization string
	ExpiresAt2   time.Time
	ErrorMessage string
}

// PendingStore is implemented by store.Store to persist pending OAuth flows.
type PendingStore interface {
	UpsertPendingOAuth(ctx context.Context, id, provider, state, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64) error
	FindPendingOAuthByState(ctx context.Context, state string) (id, provider, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64, err error)
	GetPendingOAuth(ctx context.Context, id string) (provider, state, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64, err error)
	UpdatePendingOAuthStatus(ctx context.Context, id, status, data string) error
	GCPendingOAuth(ctx context.Context) error
}

type Manager struct {
	mu      sync.Mutex
	pending map[string]*PendingAuth
	store   PendingStore
}

func New() *Manager {
	return &Manager{pending: map[string]*PendingAuth{}}
}

// SetStore enables persistent storage for pending OAuth flows.
// When set, flows survive gateway restarts.
func (m *Manager) SetStore(s PendingStore) {
	m.store = s
}

// Start creates a new OAuth flow and returns the authorize URL.
func (m *Manager) Start(provider Provider, tenantID, note string) (*PendingAuth, string, error) {
	cfg := ConfigFor(provider)
	if cfg == nil {
		return nil, "", fmt.Errorf("unknown provider: %s", provider)
	}

	// PKCE: Codex uses hex-encoded verifier (sub2api style: 64 random bytes → 128 hex chars).
	// Claude/Gemini use base64url-no-pad verifier (32 random bytes → 43 chars, RFC 7636).
	var verifier, challenge string
	var err error
	if provider == ProviderCodex {
		verifier, challenge, err = generatePKCEHex()
	} else {
		verifier, challenge, err = generatePKCE()
	}
	if err != nil {
		return nil, "", err
	}

	// State generation:
	// - Codex: hex-encoded 32 bytes (matches sub2api GenerateState → hex.EncodeToString)
	// - Claude/Gemini: base64url-no-pad 32 bytes
	var state string
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, "", err
	}
	if provider == ProviderCodex {
		const hexDigits = "0123456789abcdef"
		hexState := make([]byte, 64)
		for i, v := range stateBytes {
			hexState[i*2] = hexDigits[v>>4]
			hexState[i*2+1] = hexDigits[v&0xF]
		}
		state = string(hexState)
	} else {
		state = base64.RawURLEncoding.EncodeToString(stateBytes)
	}

	id := randHex(12)
	p := &PendingAuth{
		ID:           id,
		Provider:     provider,
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  cfg.RedirectURI,
		TenantID:     tenantID,
		Note:         note,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(30 * time.Minute),
		Status:       "pending",
	}
	m.mu.Lock()
	m.pending[id] = p
	m.gcLocked()
	m.mu.Unlock()

	// Persist to DB so flows survive restarts
	if m.store != nil {
		_ = m.store.UpsertPendingOAuth(context.Background(),
			id, string(provider), state, verifier, cfg.RedirectURI,
			tenantID, note, "pending", "",
			p.CreatedAt.Unix(), p.ExpiresAt.Unix())
	}

	var authorize string
	switch provider {
	case ProviderClaude:
		// sub2api builds Claude URL manually with fixed parameter order.
		// redirect_uri and scope must be percent-encoded in a specific way.
		encodedRedirect := url.QueryEscape(cfg.RedirectURI)
		encodedScope := strings.ReplaceAll(url.QueryEscape(cfg.Scope), "%20", "+")
		authorize = fmt.Sprintf(
			"%s?code=true&client_id=%s&response_type=code&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
			cfg.AuthorizeURL,
			cfg.ClientID,
			encodedRedirect,
			encodedScope,
			challenge,
			state,
		)
	default:
		// Codex and Gemini use standard url.Values encoding.
		q := url.Values{}
		q.Set("response_type", "code")
		q.Set("client_id", cfg.ClientID)
		q.Set("redirect_uri", cfg.RedirectURI)
		q.Set("scope", cfg.Scope)
		q.Set("state", state)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		for k, v := range cfg.ExtraParams {
			q.Set(k, v)
		}
		authorize = cfg.AuthorizeURL + "?" + q.Encode()
	}

	return p, authorize, nil
}

func (m *Manager) FindByState(state string) (*PendingAuth, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pending {
		if p.State == state && p.Status == "pending" {
			return p, true
		}
	}
	// Fallback: check persistent store (survives restarts)
	if m.store != nil {
		id, provider, cv, ruri, tid, note, status, _, createdAt, expiresAt, err :=
			m.store.FindPendingOAuthByState(context.Background(), state)
		if err == nil && status == "pending" && time.Unix(expiresAt, 0).After(time.Now()) {
			p := &PendingAuth{
				ID:           id,
				Provider:     Provider(provider),
				State:        state,
				CodeVerifier: cv,
				RedirectURI:  ruri,
				TenantID:     tid,
				Note:         note,
				CreatedAt:    time.Unix(createdAt, 0),
				ExpiresAt:    time.Unix(expiresAt, 0),
				Status:       "pending",
			}
			m.pending[id] = p
			return p, true
		}
	}
	return nil, false
}

func (m *Manager) Get(id string) (*PendingAuth, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if ok {
		return p, true
	}
	// Fallback: check persistent store
	if m.store != nil {
		provider, state, cv, ruri, tid, note, status, _, createdAt, expiresAt, err :=
			m.store.GetPendingOAuth(context.Background(), id)
		if err == nil {
			p = &PendingAuth{
				ID:           id,
				Provider:     Provider(provider),
				State:        state,
				CodeVerifier: cv,
				RedirectURI:  ruri,
				TenantID:     tid,
				Note:         note,
				CreatedAt:    time.Unix(createdAt, 0),
				ExpiresAt:    time.Unix(expiresAt, 0),
				Status:       status,
			}
			m.pending[id] = p
			return p, true
		}
	}
	return nil, false
}

// Exchange swaps the auth code for tokens.
func (m *Manager) Exchange(p *PendingAuth, authCode string) error {
	cfg := ConfigFor(p.Provider)
	if cfg == nil {
		return fmt.Errorf("unknown provider: %s", p.Provider)
	}

	// Claude uses a JSON body, Codex/Gemini use form-encoded.
	var req *http.Request
	var err error
	if p.Provider == ProviderClaude {
		body := map[string]interface{}{
			"grant_type":    "authorization_code",
			"client_id":     cfg.ClientID,
			"code":          stripStateFragment(authCode),
			"redirect_uri":  cfg.RedirectURI,
			"code_verifier": p.CodeVerifier,
			"state":         p.State,
		}
		jsonBody, _ := json.Marshal(body)
		req, err = http.NewRequest("POST", cfg.TokenURL, bytes.NewReader(jsonBody))
		if err != nil {
			m.fail(p, err.Error())
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", authCode)
		form.Set("client_id", cfg.ClientID)
		form.Set("code_verifier", p.CodeVerifier)
		form.Set("redirect_uri", cfg.RedirectURI)
		if cfg.ClientSecret != "" {
			form.Set("client_secret", cfg.ClientSecret)
		}
		req, err = http.NewRequest("POST", cfg.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			m.fail(p, err.Error())
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	switch p.Provider {
	case ProviderClaude:
		req.Header.Set("User-Agent", "axios/1.13.6")
	case ProviderCodex:
		req.Header.Set("User-Agent", "codex-cli/0.91.0")
	default:
		req.Header.Set("User-Agent", "codex-cli/0.91.0")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		m.fail(p, err.Error())
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err := fmt.Errorf("token exchange %d: %s", resp.StatusCode, string(body))
		m.fail(p, err.Error())
		return err
	}

	// Parse — different providers return slightly different shapes.
	switch p.Provider {
	case ProviderCodex:
		return m.parseCodexToken(p, body)
	case ProviderClaude:
		return m.parseClaudeToken(p, body)
	case ProviderGemini:
		return m.parseGeminiToken(p, body)
	}
	return fmt.Errorf("unhandled provider: %s", p.Provider)
}

func (m *Manager) parseCodexToken(p *PendingAuth, body []byte) error {
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		m.fail(p, "decode codex token: "+err.Error())
		return err
	}
	if tok.AccessToken == "" {
		m.fail(p, "empty access_token")
		return errors.New("empty access_token")
	}
	m.mu.Lock()
	p.AccessToken = tok.AccessToken
	p.RefreshToken = tok.RefreshToken
	p.IDToken = tok.IDToken
	if tok.ExpiresIn > 0 {
		p.ExpiresAt2 = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	// Claims live in the id_token (preferred) or access_token.
	if tok.IDToken != "" {
		p.AccountID, p.PlanType, p.Email = extractCodexClaims(tok.IDToken)
	}
	if p.AccountID == "" && tok.AccessToken != "" {
		a, plan, em := extractCodexClaims(tok.AccessToken)
		if p.AccountID == "" {
			p.AccountID = a
		}
		if p.PlanType == "" {
			p.PlanType = plan
		}
		if p.Email == "" {
			p.Email = em
		}
	}
	p.Status = "success"
	m.mu.Unlock()
	m.markSuccess(p)
	return nil
}

func (m *Manager) parseClaudeToken(p *PendingAuth, body []byte) error {
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Organization struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"organization"`
		Account struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		m.fail(p, "decode claude token: "+err.Error())
		return err
	}
	if tok.AccessToken == "" {
		m.fail(p, "empty access_token")
		return errors.New("empty access_token")
	}
	m.mu.Lock()
	p.AccessToken = tok.AccessToken
	p.RefreshToken = tok.RefreshToken
	p.AccountID = tok.Account.UUID
	p.Email = tok.Account.EmailAddress
	p.Organization = tok.Organization.UUID
	if tok.ExpiresIn > 0 {
		p.ExpiresAt2 = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	p.Status = "success"
	m.mu.Unlock()
	m.markSuccess(p)
	return nil
}

func (m *Manager) parseGeminiToken(p *PendingAuth, body []byte) error {
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		m.fail(p, "decode gemini token: "+err.Error())
		return err
	}
	m.mu.Lock()
	p.AccessToken = tok.AccessToken
	p.RefreshToken = tok.RefreshToken
	p.IDToken = tok.IDToken
	if tok.IDToken != "" {
		// Google id_token is a JWT with email claim.
		_, _, p.Email = extractGoogleClaims(tok.IDToken)
	}
	if tok.ExpiresIn > 0 {
		p.ExpiresAt2 = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	p.Status = "success"
	m.mu.Unlock()
	m.markSuccess(p)
	return nil
}

// BuildSessionJSON renders the obtained tokens into a JSON shape that the
// chatgpt provider's session resolver understands. For Claude/Gemini we use
// shapes their own provider modules will recognise.
func (m *Manager) BuildSessionJSON(p *PendingAuth) string {
	switch p.Provider {
	case ProviderCodex:
		return m.buildCodexSession(p)
	case ProviderClaude:
		return m.buildClaudeSession(p)
	case ProviderGemini:
		return m.buildGeminiSession(p)
	}
	return ""
}

func (m *Manager) buildCodexSession(p *PendingAuth) string {
	out := map[string]interface{}{
		"user": map[string]string{
			"id":    p.AccountID,
			"email": p.Email,
		},
		"expires": time.Now().Add(60 * 24 * time.Hour).Format(time.RFC3339),
		"account": map[string]string{
			"id":       p.AccountID,
			"planType": p.PlanType,
		},
		"accessToken":  p.AccessToken,
		"refreshToken": p.RefreshToken,
		"idToken":      p.IDToken,
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (m *Manager) buildClaudeSession(p *PendingAuth) string {
	out := map[string]interface{}{
		"access_token":      p.AccessToken,
		"refresh_token":     p.RefreshToken,
		"organization_uuid": p.Organization,
		"account_uuid":      p.AccountID,
		"email":             p.Email,
		"expires_at":        p.ExpiresAt2.Format(time.RFC3339),
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (m *Manager) buildGeminiSession(p *PendingAuth) string {
	out := map[string]interface{}{
		"access_token":  p.AccessToken,
		"refresh_token": p.RefreshToken,
		"id_token":      p.IDToken,
		"email":         p.Email,
		"expires_at":    p.ExpiresAt2.Format(time.RFC3339),
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (m *Manager) fail(p *PendingAuth, errMsg string) {
	m.mu.Lock()
	p.Status = "failed"
	p.ErrorMessage = errMsg
	m.mu.Unlock()
	if m.store != nil {
		_ = m.store.UpdatePendingOAuthStatus(context.Background(), p.ID, "failed", errMsg)
	}
}

func (m *Manager) markSuccess(p *PendingAuth) {
	if m.store != nil {
		_ = m.store.UpdatePendingOAuthStatus(context.Background(), p.ID, "success", "")
	}
}

func (m *Manager) gcLocked() {
	now := time.Now()
	for k, v := range m.pending {
		if now.After(v.ExpiresAt.Add(1 * time.Hour)) {
			delete(m.pending, k)
		}
	}
}

// ----- helpers -----

// generatePKCE generates verifier (96 random bytes → base64url-nopad ~128 chars)
// and S256 challenge. Used for Claude and Gemini (RFC 7636 standard).
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 96)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(h[:])
	return verifier, challenge, nil
}

// generatePKCEHex generates verifier as hex (64 random bytes → 128 hex chars)
// and S256 challenge as base64url-nopad. Used for Codex (sub2api style).
func generatePKCEHex() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	const hexDigits = "0123456789abcdef"
	hex := make([]byte, 128)
	for i, v := range b {
		hex[i*2] = hexDigits[v>>4]
		hex[i*2+1] = hexDigits[v&0xF]
	}
	verifier = string(hex)
	h := sha256.Sum256([]byte(verifier))
	// base64url without padding, strip trailing '='
	challenge = strings.TrimRight(base64.URLEncoding.EncodeToString(h[:]), "=")
	return verifier, challenge, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	out := make([]byte, n*2)
	const hexDigits = "0123456789abcdef"
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0xF]
	}
	return string(out)
}

func stripStateFragment(code string) string {
	// Anthropic occasionally returns "code#state" combined.
	if idx := strings.Index(code, "#"); idx >= 0 {
		return code[:idx]
	}
	return code
}

// extractCodexClaims pulls account_id / plan_type / email out of a Codex
// JWT payload (without verifying signature; the issuer already validated it).
func extractCodexClaims(jwt string) (accountID, planType, email string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(addB64Padding(parts[1]))
	if err != nil {
		// try with standard padding
		payload, err = base64.URLEncoding.DecodeString(addB64Padding(parts[1]))
		if err != nil {
			return
		}
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(payload, &nested); err != nil {
		return
	}
	if a, ok := nested["https://api.openai.com/auth"]; ok {
		var inner struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		}
		_ = json.Unmarshal(a, &inner)
		accountID = inner.ChatGPTAccountID
		planType = inner.ChatGPTPlanType
	}
	if e, ok := nested["email"]; ok {
		_ = json.Unmarshal(e, &email)
	}
	return
}

func extractGoogleClaims(jwt string) (sub, name, email string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(addB64Padding(parts[1]))
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(addB64Padding(parts[1]))
		if err != nil {
			return
		}
	}
	var c struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	_ = json.Unmarshal(payload, &c)
	return c.Sub, c.Name, c.Email
}

func addB64Padding(s string) string {
	switch len(s) % 4 {
	case 2:
		return s + "=="
	case 3:
		return s + "="
	}
	return s
}

// RefreshCodex refreshes a Codex access token using the stored refresh token.
// Used by the chatgpt provider when the cached access_token is close to
// expiring.
func (m *Manager) RefreshCodex(ctx context.Context, refreshToken string) (newAccess, newRefresh, idToken string, expiresIn int, err error) {
	cfg := &CodexConfig
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", "openid profile email") // sub2api RefreshScopes order
	req, _ := http.NewRequestWithContext(ctx, "POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli/0.91.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", "", 0, fmt.Errorf("refresh %d: %s", resp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", "", "", 0, err
	}
	if tok.RefreshToken == "" {
		// Some providers don't return a new refresh token; reuse.
		tok.RefreshToken = refreshToken
	}
	return tok.AccessToken, tok.RefreshToken, tok.IDToken, tok.ExpiresIn, nil
}
