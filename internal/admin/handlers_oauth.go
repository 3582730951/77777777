package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/enrollment"
	"github.com/llm-pool/gateway/internal/oauth"
	chatgptprovider "github.com/llm-pool/gateway/internal/provider/chatgpt"
	"github.com/llm-pool/gateway/internal/store"
)

// SetOAuth wires the OAuth manager (called from main.go).
func (s *Server) SetOAuth(m *oauth.Manager) { s.oauth = m }

// StartLocalCallbackListeners starts every configured OAuth callback listener.
// Same handler logic for all — they look up the pending flow by `state` and
// exchange the code.
func (s *Server) StartLocalCallbackListeners() {
	if s.oauth == nil {
		return
	}
	for _, cfg := range oauth.AllProviders() {
		go s.runCallbackListener(cfg)
	}
}

func (s *Server) runCallbackListener(cfg oauth.ProviderConfig) {
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		s.handleProviderCallback(w, r, cfg)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Some providers append fragments before the path; redirect to the
		// canonical callback path with original query so the same handler
		// catches it.
		http.Redirect(w, r, cfg.CallbackPath+"?"+r.URL.RawQuery, http.StatusSeeOther)
	})
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.CallbackPort),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		s.deps.Logger.Warn("oauth callback listener", "provider", cfg.ID, "port", cfg.CallbackPort, "err", err)
	}
}

func (s *Server) handleProviderCallback(w http.ResponseWriter, r *http.Request, cfg oauth.ProviderConfig) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		if relayURL, ok := oauth.RelayCallbackURL(cfg.ID, r.URL.Query()); ok {
			http.Redirect(w, r, relayURL, http.StatusSeeOther)
			return
		}
		renderCallbackPage(w, "error", "OAuth provider returned: "+errParam+" - "+r.URL.Query().Get("error_description"))
		return
	}
	if state == "" || code == "" {
		renderCallbackPage(w, "error", "Missing state or code in callback URL.")
		return
	}
	p, ok := s.oauth.FindByState(state)
	if !ok {
		if relayURL, ok := oauth.RelayCallbackURL(cfg.ID, r.URL.Query()); ok {
			http.Redirect(w, r, relayURL, http.StatusSeeOther)
			return
		}
		renderCallbackPage(w, "error", "Unknown or expired state token. Please restart enrollment from the admin UI.")
		return
	}
	if string(p.Provider) != string(cfg.ID) {
		renderCallbackPage(w, "error", "State/provider mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.completeOAuthCallback(ctx, p, code); err != nil {
		renderCallbackPage(w, "error", err.Error())
		return
	}
	renderCallbackPage(w, "ok", string(cfg.ID)+" account enrolled. You can close this tab; the admin UI will auto-redirect.")
}

func (s *Server) handleOAuthRelayCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		renderCallbackPage(w, "error", "OAuth manager not wired.")
		return
	}
	provider := oauth.Provider(r.URL.Query().Get("provider"))
	cfg := oauth.ConfigFor(provider)
	if cfg == nil {
		renderCallbackPage(w, "error", "Unknown OAuth provider.")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		renderCallbackPage(w, "error", "OAuth provider returned: "+errParam+" - "+r.URL.Query().Get("error_description"))
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		renderCallbackPage(w, "error", "Missing state or code in callback URL.")
		return
	}
	p, ok := s.oauth.FindByState(state)
	if !ok {
		renderCallbackPage(w, "error", "Unknown or expired state token. Please restart enrollment from the admin UI.")
		return
	}
	if p.Provider != provider {
		renderCallbackPage(w, "error", "State/provider mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.completeOAuthCallback(ctx, p, code); err != nil {
		renderCallbackPage(w, "error", err.Error())
		return
	}
	renderCallbackPage(w, "ok", string(cfg.ID)+" account enrolled. You can close this tab; the admin UI will auto-redirect.")
}

func (s *Server) completeOAuthCallback(ctx context.Context, p *oauth.PendingAuth, code string) error {
	if err := s.oauth.Exchange(p, code); err != nil {
		return errors.New("Token exchange failed: " + err.Error())
	}
	if err := s.persistOAuthAccount(ctx, p); err != nil {
		return errors.New("Save account failed: " + err.Error())
	}
	return nil
}

func renderCallbackPage(w http.ResponseWriter, kind, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	color := "#30d158"
	title := "✓ 录入成功"
	if kind == "error" {
		color = "#ff453a"
		title = "✗ 录入失败"
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>OAuth callback</title>
<style>body{font-family:-apple-system,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f5f7}
.card{background:#fff;padding:40px;border-radius:18px;box-shadow:0 24px 48px -12px rgba(0,0,0,0.12);max-width:480px;text-align:center}
h1{color:%s;margin:0 0 12px;font-size:22px}
p{color:#6e6e73;font-size:14px;line-height:1.5}</style></head>
<body><div class="card"><h1>%s</h1><p>%s</p></div></body></html>`, color, title, msg)
}

func (s *Server) persistOAuthAccount(ctx context.Context, p *oauth.PendingAuth) error {
	sessionJSON := s.oauth.BuildSessionJSON(p)
	id := "acc-" + randHex(6)
	provider := string(p.Provider)
	// Map our internal provider names: "codex" → "chatgpt" since we route
	// codex via /backend-api/codex/responses inside the chatgpt provider.
	switch p.Provider {
	case oauth.ProviderCodex:
		provider = "chatgpt"
	case oauth.ProviderClaude:
		provider = "claude"
	case oauth.ProviderGemini:
		provider = "gemini"
	case oauth.ProviderKiro:
		provider = "kiro"
	}
	a := &domain.Account{
		ID:             id,
		TenantID:       p.TenantID,
		Provider:       provider,
		Email:          p.Email,
		PlanTier:       p.PlanType,
		StealthProfile: "chrome_124_windows",
		UA:             "codex_cli_rs/0.45.0",
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		SessionToken: sessionJSON,
		RefreshToken: p.RefreshToken,
	}
	if p.Provider == oauth.ProviderKiro {
		a.PlanTier = defaultStr(a.PlanTier, "kiro")
		a.UA = "KiroIDE/0.11.63"
		sec.Cookies, _ = json.Marshal(map[string]string{
			"client_id":     p.OAuthClientID,
			"client_secret": p.OAuthClientSecret,
			"profile_arn":   p.ProfileArn,
			"region":        p.OAuthRegion,
		})
	}
	if err := s.deps.Store.UpsertAccount(ctx, a, sec); err != nil {
		return err
	}
	s.deps.Sched.Register(a)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "oauth", id, "", "OAuth enrollment completed: provider="+string(p.Provider)+" tenant="+p.TenantID+" plan="+p.PlanType+" email="+p.Email)
	}
	p.AccountID = id
	return nil
}

// ---- Admin handlers ----

func (s *Server) handleOAuthStartGet(w http.ResponseWriter, r *http.Request) {
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	s.render(w, r, "oauth_start.html", map[string]any{
		"Active":      "accounts",
		"Title":       s.t(r, "enroll.title"),
		"YamlTenants": s.deps.Cfg.Tenants,
		"Tenants":     tenants,
		"Providers":   oauth.AllProviders(),
	})
}

func (s *Server) handleOAuthStartPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	loginMode := strings.TrimSpace(r.FormValue("login_mode"))
	provider := oauth.Provider(r.FormValue("provider"))
	if oauth.ConfigFor(provider) == nil {
		provider = oauth.ProviderCodex
	}
	tenantID := r.FormValue("tenant_id")
	if tenantID == "" {
		tenantID = "default"
	}
	note := r.FormValue("note")
	workspaceID := strings.TrimSpace(r.FormValue("workspace_id"))
	if loginMode == "web_session" {
		if s.enroll == nil {
			http.Error(w, "enrollment manager not wired", 500)
			return
		}
		pending := s.enroll.Create(tenantID, "chatgpt", "", note)
		http.Redirect(w, r, "/accounts/oauth/web-session/"+pending.ID, http.StatusSeeOther)
		return
	}
	if s.oauth == nil {
		http.Error(w, "oauth manager not wired", 500)
		return
	}
	p, authURL, err := s.oauth.StartWithOptions(provider, tenantID, note, adminPublicBaseURL(r), workspaceID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	encoded := url.QueryEscape(authURL)
	http.Redirect(w, r, "/accounts/oauth/"+p.ID+"?u="+encoded, http.StatusSeeOther)
}

func (s *Server) handleOAuthShow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := s.oauth.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	authURL := oauthShowAuthURL(r)
	if authURL == "" {
		authURL = "(start a new enrollment to get a fresh URL)"
	}
	cfg := oauth.ConfigFor(p.Provider)
	openURL := oauthOpenURL(p.ID, authURL, false)
	s.render(w, r, "oauth_show.html", map[string]any{
		"Active":        "accounts",
		"Title":         s.t(r, "enroll.title"),
		"Pending":       p,
		"AuthURL":       authURL,
		"OpenURL":       openURL,
		"DirectOpenURL": oauthOpenURL(p.ID, authURL, true),
		"Cfg":           cfg,
	})
}

func oauthShowAuthURL(r *http.Request) string {
	// r.URL.Query() has already decoded the outer admin `u=` transport
	// parameter. Do not decode again: the inner OAuth URL must keep its own
	// percent-encoding (`redirect_uri`, `scope`, etc.) exactly as generated.
	return strings.TrimSpace(r.URL.Query().Get("u"))
}

func oauthOpenURL(id, authURL string, direct bool) string {
	authURL = strings.TrimSpace(authURL)
	if id == "" || authURL == "" || strings.HasPrefix(authURL, "(") {
		return "#"
	}
	v := url.Values{}
	v.Set("u", authURL)
	if direct {
		v.Set("direct", "1")
	}
	return "/accounts/oauth/" + url.PathEscape(id) + "/open?" + v.Encode()
}

func (s *Server) handleOAuthOpen(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.Error(w, "oauth manager not wired", http.StatusInternalServerError)
		return
	}
	id := chi.URLParam(r, "id")
	p, ok := s.oauth.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	authURL := oauthShowAuthURL(r)
	if authURL == "" {
		http.Error(w, "auth URL required", http.StatusBadRequest)
		return
	}
	cfg := oauth.ConfigFor(p.Provider)
	if !oauthAuthorizeURLValid(authURL, p, cfg) {
		http.Error(w, "auth URL does not match this enrollment", http.StatusBadRequest)
		return
	}
	if cfg == nil || p.Provider != oauth.ProviderCodex || r.URL.Query().Get("direct") == "1" {
		http.Redirect(w, r, authURL, http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "oauth_open.html", map[string]any{
		"Active":        "accounts",
		"Title":         s.t(r, "enroll.title"),
		"Pending":       p,
		"AuthURL":       authURL,
		"ShowURL":       oauthShowURL(p.ID, authURL),
		"DirectOpenURL": oauthOpenURL(p.ID, authURL, true),
		"Cfg":           cfg,
	})
}

func oauthShowURL(id, authURL string) string {
	authURL = strings.TrimSpace(authURL)
	if id == "" || authURL == "" {
		return "/accounts/oauth"
	}
	return "/accounts/oauth/" + url.PathEscape(id) + "?u=" + url.QueryEscape(authURL)
}

func oauthAuthorizeURLValid(authURL string, p *oauth.PendingAuth, cfg *oauth.ProviderConfig) bool {
	if p == nil || cfg == nil {
		return false
	}
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	base, err := url.Parse(cfg.AuthorizeURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return false
	}
	if strings.TrimRight(u.Path, "/") != strings.TrimRight(base.Path, "/") {
		return false
	}
	if state := u.Query().Get("state"); state != "" && state != p.State {
		return false
	}
	return true
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := s.oauth.Get(id)
	if !ok {
		errJSON(w, 404, "not found")
		return
	}
	writeJSONStatus(w, 200, map[string]any{
		"id":         p.ID,
		"status":     p.Status,
		"provider":   string(p.Provider),
		"account_id": p.AccountID,
		"error":      p.ErrorMessage,
		"plan":       p.PlanType,
		"email":      p.Email,
	})
}

func (s *Server) handleOAuthPasteCallback(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := s.oauth.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pasted := strings.TrimSpace(r.FormValue("callback_url"))
	if pasted == "" {
		http.Error(w, "callback_url required", 400)
		return
	}
	u, err := url.Parse(pasted)
	if err != nil {
		http.Error(w, "bad URL: "+err.Error(), 400)
		return
	}
	state := u.Query().Get("state")
	code := u.Query().Get("code")
	if state != p.State {
		http.Error(w, "state mismatch (this URL is for a different enrollment)", 400)
		return
	}
	if code == "" {
		http.Error(w, "missing code in pasted URL", 400)
		return
	}
	if err := s.oauth.Exchange(p, code); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if err := s.persistOAuthAccount(r.Context(), p); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/accounts/"+p.AccountID, http.StatusSeeOther)
}

func (s *Server) handleOAuthWebSessionShow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.enroll == nil {
		http.Error(w, "enrollment manager not wired", 500)
		return
	}
	p, ok := s.enroll.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	host := publicHost(r)
	postURL := fmt.Sprintf("%s/enroll/%s", host, id)
	s.render(w, r, "oauth_web_session.html", map[string]any{
		"Active":      "accounts",
		"Title":       s.t(r, "enroll.title"),
		"Pending":     p,
		"PostURL":     postURL,
		"Bookmarklet": buildBookmarklet("chatgpt", postURL),
	})
}

func (s *Server) handleOAuthWebSessionStatus(w http.ResponseWriter, r *http.Request) {
	s.handleAccountEnrollStatus(w, r)
}

func (s *Server) handleOAuthWebSessionSubmit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.enroll == nil {
		http.Error(w, "enrollment manager not wired", 500)
		return
	}
	p, ok := s.enroll.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if p.State != enrollment.StatePending {
		http.Error(w, "enrollment "+string(p.State), 410)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sessionJSON := strings.TrimSpace(r.FormValue("session"))
	cookies := strings.TrimSpace(r.FormValue("cookies"))
	ua := strings.TrimSpace(r.FormValue("ua"))
	requireCookies := true
	switch strings.TrimSpace(r.FormValue("import_mode")) {
	case "", "web_session":
	case "session_only_json", "session_only", "auth_json", "cpa_auth_json":
		normalized, err := chatgptprovider.NormalizeSessionOnlyAuthJSON(sessionJSON)
		if err != nil {
			s.enroll.Fail(id, err.Error())
			http.Error(w, err.Error(), 400)
			return
		}
		sessionJSON = normalized
		cookies = ""
		requireCookies = false
	default:
		err := fmt.Errorf("unknown web session import mode")
		s.enroll.Fail(id, err.Error())
		http.Error(w, err.Error(), 400)
		return
	}
	accID, err := s.completeWebSessionEnrollment(r.Context(), p, sessionJSON, cookies, ua, requireCookies)
	if err != nil {
		s.enroll.Fail(id, err.Error())
		http.Error(w, err.Error(), 400)
		return
	}
	s.enroll.Complete(id, accID)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "oauth-web-session", accID, "", "ChatGPT web session enrollment completed: tenant="+p.TenantID+" email="+accountEmailFromImport("", sessionJSON))
	}
	http.Redirect(w, r, "/accounts/"+accID, http.StatusSeeOther)
}

func adminPublicBaseURL(r *http.Request) string {
	proto := firstCSV(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on") || strings.EqualFold(r.Header.Get("X-Forwarded-Scheme"), "https") {
			proto = "https"
		} else if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := firstCSV(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	prefix := strings.TrimRight(firstCSV(r.Header.Get("X-Forwarded-Prefix")), "/")
	if prefix == "/" {
		prefix = ""
	}
	return strings.TrimRight(proto+"://"+host+prefix, "/")
}

func firstCSV(value string) string {
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

var _ = json.Marshal
var _ = errors.New
