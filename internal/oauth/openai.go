// openai.go (renamed conceptually to manager.go) — generic PKCE OAuth flow
// driver. Works for any of the three providers defined in providers.go.
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	ID                string
	Provider          Provider
	State             string
	CodeVerifier      string
	RedirectURI       string
	TenantID          string
	Note              string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	Status            string // pending / success / failed
	AccessToken       string
	RefreshToken      string
	IDToken           string
	AccountID         string
	Email             string
	PlanType          string
	Organization      string
	ProfileArn        string
	OAuthClientID     string
	OAuthClientSecret string
	OAuthRegion       string
	WorkspaceID       string
	ExpiresAt2        time.Time
	ErrorMessage      string
}

type pendingAuthData struct {
	OAuthClientID     string `json:"oauth_client_id,omitempty"`
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	OAuthRegion       string `json:"oauth_region,omitempty"`
	WorkspaceID       string `json:"workspace_id,omitempty"`
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
	return m.StartWithRelayBase(provider, tenantID, note, "")
}

// StartWithRelayBase creates a new OAuth flow and embeds the admin origin in
// state so a localhost callback handled by another process can relay the code
// back to the server that created the enrollment.
func (m *Manager) StartWithRelayBase(provider Provider, tenantID, note, relayBase string) (*PendingAuth, string, error) {
	return m.StartWithOptions(provider, tenantID, note, relayBase, "")
}

// StartWithOptions creates a new OAuth flow with provider-specific optional
// inputs. workspaceID is currently used by Codex as allowed_workspace_id.
func (m *Manager) StartWithOptions(provider Provider, tenantID, note, relayBase, workspaceID string) (*PendingAuth, string, error) {
	cfg := ConfigFor(provider)
	if cfg == nil {
		return nil, "", fmt.Errorf("unknown provider: %s", provider)
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if provider != ProviderCodex {
		workspaceID = ""
	}

	// PKCE: Codex follows the official CLI: 64 random bytes → base64url verifier.
	// Claude/Gemini use the shared RFC 7636 helper.
	var verifier, challenge string
	var err error
	if provider == ProviderCodex {
		verifier, challenge, err = generatePKCEBytes(64)
	} else {
		verifier, challenge, err = generatePKCE()
	}
	if err != nil {
		return nil, "", err
	}

	state, err := generateOAuthState()
	if err != nil {
		return nil, "", err
	}
	if provider != ProviderCodex {
		state = encodeRelayState(provider, state, relayBase)
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
		WorkspaceID:  workspaceID,
	}

	var authorize string
	switch provider {
	case ProviderCodex:
		authorize = buildCodexAuthorizeURL(cfg, challenge, state, workspaceID)
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
	case ProviderKiro:
		region := cfg.ExtraParams["region"]
		if region == "" {
			region = "us-east-1"
		}
		clientID, clientSecret, err := registerKiroOIDCClient(context.Background(), region, cfg.RedirectURI)
		if err != nil {
			return nil, "", err
		}
		p.OAuthClientID = clientID
		p.OAuthClientSecret = clientSecret
		p.OAuthRegion = region

		q := url.Values{}
		q.Set("response_type", "code")
		q.Set("client_id", clientID)
		q.Set("redirect_uri", cfg.RedirectURI)
		q.Set("scopes", cfg.Scope)
		q.Set("state", state)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		authorize = cfg.AuthorizeURL + "?" + q.Encode()
	default:
		// Gemini uses standard url.Values encoding.
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

	m.mu.Lock()
	m.pending[id] = p
	m.gcLocked()
	m.mu.Unlock()

	// Persist to DB so flows survive restarts.
	if m.store != nil {
		_ = m.store.UpsertPendingOAuth(context.Background(),
			id, string(provider), state, verifier, cfg.RedirectURI,
			tenantID, note, "pending", p.dataJSON(),
			p.CreatedAt.Unix(), p.ExpiresAt.Unix())
	}

	return p, authorize, nil
}

type codexQueryParam struct {
	key   string
	value string
}

func buildCodexAuthorizeURL(cfg *ProviderConfig, challenge, state, workspaceID string) string {
	params := []codexQueryParam{
		{"response_type", "code"},
		{"client_id", cfg.ClientID},
		{"redirect_uri", cfg.RedirectURI},
		{"scope", cfg.Scope},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"id_token_add_organizations", cfg.ExtraParams["id_token_add_organizations"]},
		{"codex_cli_simplified_flow", cfg.ExtraParams["codex_cli_simplified_flow"]},
		{"state", state},
		{"originator", cfg.ExtraParams["originator"]},
	}
	if workspaceID != "" {
		params = append(params, codexQueryParam{"allowed_workspace_id", workspaceID})
	}
	var b strings.Builder
	for i, param := range params {
		if param.key == "" || param.value == "" {
			continue
		}
		if i > 0 && b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(codexQueryEscape(param.key))
		b.WriteByte('=')
		b.WriteString(codexQueryEscape(param.value))
	}
	return cfg.AuthorizeURL + "?" + b.String()
}

func codexQueryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func (p *PendingAuth) dataJSON() string {
	data := pendingAuthData{
		OAuthClientID:     p.OAuthClientID,
		OAuthClientSecret: p.OAuthClientSecret,
		OAuthRegion:       p.OAuthRegion,
		WorkspaceID:       p.WorkspaceID,
	}
	if data.OAuthClientID == "" && data.OAuthClientSecret == "" && data.OAuthRegion == "" && data.WorkspaceID == "" {
		return ""
	}
	b, _ := json.Marshal(data)
	return string(b)
}

func (p *PendingAuth) applyData(data string) {
	if data == "" {
		return
	}
	var d pendingAuthData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return
	}
	p.OAuthClientID = d.OAuthClientID
	p.OAuthClientSecret = d.OAuthClientSecret
	p.OAuthRegion = d.OAuthRegion
	p.WorkspaceID = d.WorkspaceID
}

var registerKiroOIDCClient = registerKiroOIDCClientDefault

func registerKiroOIDCClientDefault(ctx context.Context, region, redirectURI string) (clientID, clientSecret string, err error) {
	if region == "" {
		region = "us-east-1"
	}
	cfg := ConfigFor(ProviderKiro)
	if cfg == nil {
		return "", "", errors.New("kiro config missing")
	}
	scopes := strings.Fields(cfg.Scope)
	payload, _ := json.Marshal(map[string]any{
		"clientName":   "llm-pool-kiro",
		"clientType":   "public",
		"scopes":       scopes,
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"issuerUrl":    cfg.ExtraParams["issuer_url"],
		"redirectUris": []string{redirectURI},
	})
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/client/register", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("kiro oidc register: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("kiro oidc register %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("decode kiro oidc register: %w", err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return "", "", errors.New("kiro oidc register returned empty client credentials")
	}
	return out.ClientID, out.ClientSecret, nil
}

func encodeRelayState(provider Provider, state, relayBase string) string {
	relayBase = strings.TrimRight(strings.TrimSpace(relayBase), "/")
	if relayBase == "" {
		return state
	}
	u, err := url.Parse(relayBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return state
	}
	switch provider {
	case ProviderCodex:
		return state + hex.EncodeToString([]byte(relayBase))
	default:
		return state + "." + base64.RawURLEncoding.EncodeToString([]byte(relayBase))
	}
}

func RelayBaseFromState(provider Provider, state string) (string, bool) {
	var raw []byte
	var err error
	switch provider {
	case ProviderCodex:
		if len(state) <= 64 || (len(state)-64)%2 != 0 {
			return "", false
		}
		raw, err = hex.DecodeString(state[64:])
	default:
		idx := strings.LastIndex(state, ".")
		if idx < 0 || idx == len(state)-1 {
			return "", false
		}
		raw, err = base64.RawURLEncoding.DecodeString(state[idx+1:])
	}
	if err != nil || len(raw) == 0 {
		return "", false
	}
	base := strings.TrimRight(string(raw), "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return base, true
}

func RelayCallbackURL(provider Provider, callbackQuery url.Values) (string, bool) {
	state := callbackQuery.Get("state")
	if state == "" {
		return "", false
	}
	base, ok := RelayBaseFromState(provider, state)
	if !ok {
		return "", false
	}
	q := url.Values{}
	for _, key := range []string{"code", "state", "error", "error_description"} {
		if values, ok := callbackQuery[key]; ok {
			for _, value := range values {
				q.Add(key, value)
			}
		}
	}
	q.Set("provider", string(provider))
	return base + "/accounts/oauth/callback/relay?" + q.Encode(), true
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
		id, provider, cv, ruri, tid, note, status, data, createdAt, expiresAt, err :=
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
			p.applyData(data)
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
		provider, state, cv, ruri, tid, note, status, data, createdAt, expiresAt, err :=
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
			p.applyData(data)
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

	// Claude and Kiro use JSON bodies, Codex/Gemini use form-encoded.
	var req *http.Request
	var err error
	switch p.Provider {
	case ProviderClaude:
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
	case ProviderKiro:
		if p.OAuthClientID == "" || p.OAuthClientSecret == "" {
			err := errors.New("kiro oauth client credentials missing; restart enrollment")
			m.fail(p, err.Error())
			return err
		}
		body := map[string]interface{}{
			"clientId":     p.OAuthClientID,
			"clientSecret": p.OAuthClientSecret,
			"grantType":    "authorization_code",
			"code":         stripStateFragment(authCode),
			"codeVerifier": p.CodeVerifier,
			"redirectUri":  cfg.RedirectURI,
		}
		jsonBody, _ := json.Marshal(body)
		req, err = http.NewRequest("POST", cfg.TokenURL, bytes.NewReader(jsonBody))
		if err != nil {
			m.fail(p, err.Error())
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	default:
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
	case ProviderKiro:
		req.Header.Set("User-Agent", "KiroIDE/0.11.63")
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
	case ProviderKiro:
		return m.parseKiroToken(p, body)
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

func (m *Manager) parseKiroToken(p *PendingAuth, body []byte) error {
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
		SnakeAccess  string `json:"access_token"`
		SnakeRefresh string `json:"refresh_token"`
		SnakeProfile string `json:"profile_arn"`
		SnakeExpires int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		m.fail(p, "decode kiro token: "+err.Error())
		return err
	}
	if tok.AccessToken == "" {
		tok.AccessToken = tok.SnakeAccess
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = tok.SnakeRefresh
	}
	if tok.ProfileArn == "" {
		tok.ProfileArn = tok.SnakeProfile
	}
	if tok.ExpiresIn == 0 {
		tok.ExpiresIn = tok.SnakeExpires
	}
	if tok.AccessToken == "" {
		m.fail(p, "empty accessToken")
		return errors.New("empty accessToken")
	}
	if tok.RefreshToken == "" {
		m.fail(p, "empty refreshToken")
		return errors.New("empty refreshToken")
	}
	m.mu.Lock()
	p.AccessToken = tok.AccessToken
	p.RefreshToken = tok.RefreshToken
	p.ProfileArn = tok.ProfileArn
	p.PlanType = "kiro"
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	p.ExpiresAt2 = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
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
	case ProviderKiro:
		return m.buildKiroSession(p)
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

func (m *Manager) buildKiroSession(p *PendingAuth) string {
	out := map[string]interface{}{
		"accessToken":   p.AccessToken,
		"refreshToken":  p.RefreshToken,
		"profileArn":    p.ProfileArn,
		"client_id":     p.OAuthClientID,
		"client_secret": p.OAuthClientSecret,
		"region":        p.OAuthRegion,
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
	return generatePKCEBytes(96)
}

func generatePKCEBytes(n int) (verifier, challenge string, err error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(h[:])
	return verifier, challenge, nil
}

func generateOAuthState() (string, error) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(stateBytes), nil
}

// generatePKCEHex generates verifier as hex (64 random bytes → 128 hex chars)
// and S256 challenge as base64url-nopad. Kept for compatibility with older
// sub2api-style tests/tools; new Codex login uses generatePKCEBytes(64).
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
	return m.RefreshCodexWithClient(ctx, refreshToken, nil)
}

func (m *Manager) RefreshCodexWithClient(ctx context.Context, refreshToken string, client *http.Client) (newAccess, newRefresh, idToken string, expiresIn int, err error) {
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
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", "", 0, fmt.Errorf("refresh %d: %s", resp.StatusCode, formatCodexRefreshError(resp.StatusCode, body))
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

func formatCodexRefreshError(status int, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		code := nestedString(raw, "code")
		msg := nestedString(raw, "message")
		if code == "" {
			if errCode := stringValue(raw["error"]); isCodexRefreshErrorCode(errCode) {
				code = errCode
			}
		}
		if errObj, ok := raw["error"].(map[string]any); ok {
			if code == "" {
				code = nestedString(errObj, "code")
			}
			if msg == "" {
				msg = nestedString(errObj, "message")
			}
			if code == "" {
				code = stringValue(errObj["error"])
			}
			if msg == "" {
				msg = stringValue(errObj["error_description"])
			}
		}
		if msg == "" {
			msg = stringValue(raw["error_description"])
		}
		if msg == "" {
			msg = stringValue(raw["error"])
		}
		switch strings.ToLower(code) {
		case "refresh_token_reused":
			if msg == "" {
				msg = "refresh_token has already been used; sign in again"
			}
			return strings.TrimSpace(code + ": " + msg)
		case "refresh_token_expired", "refresh_token_invalidated", "invalid_grant":
			if msg == "" {
				return code
			}
			return strings.TrimSpace(code + ": " + msg)
		}
		if code != "" && msg != "" {
			return strings.TrimSpace(code + ": " + msg)
		}
		if code != "" {
			return code
		}
		if msg != "" {
			return msg
		}
	}
	if trimmed == "" {
		return http.StatusText(status)
	}
	return trimmed
}

func isCodexRefreshErrorCode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated", "invalid_grant":
		return true
	default:
		return false
	}
}

func nestedString(m map[string]any, key string) string {
	return strings.TrimSpace(stringValue(m[key]))
}

func stringValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}
