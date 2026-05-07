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
	"github.com/llm-pool/gateway/internal/oauth"
	"github.com/llm-pool/gateway/internal/store"
)

// SetOAuth wires the OAuth manager (called from main.go).
func (s *Server) SetOAuth(m *oauth.Manager) { s.oauth = m }

// StartLocalCallbackListeners starts the three OAuth callback HTTP listeners
// (Codex :1455, Claude :54545, Gemini :8085). Same handler logic for all —
// they look up the pending flow by `state` and exchange the code.
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
		renderCallbackPage(w, "error", "OAuth provider returned: "+errParam+" - "+r.URL.Query().Get("error_description"))
		return
	}
	if state == "" || code == "" {
		renderCallbackPage(w, "error", "Missing state or code in callback URL.")
		return
	}
	p, ok := s.oauth.FindByState(state)
	if !ok {
		renderCallbackPage(w, "error", "Unknown or expired state token. Please restart enrollment from the admin UI.")
		return
	}
	if string(p.Provider) != string(cfg.ID) {
		renderCallbackPage(w, "error", "State/provider mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.oauth.Exchange(p, code); err != nil {
		renderCallbackPage(w, "error", "Token exchange failed: "+err.Error())
		return
	}
	if err := s.persistOAuthAccount(ctx, p); err != nil {
		renderCallbackPage(w, "error", "Save account failed: "+err.Error())
		return
	}
	renderCallbackPage(w, "ok", string(cfg.ID)+" account enrolled. You can close this tab; the admin UI will auto-redirect.")
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
	}
	a := &domain.Account{
		ID:             id,
		TenantID:       p.TenantID,
		Provider:       provider,
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
	if s.oauth == nil {
		http.Error(w, "oauth manager not wired", 500)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	provider := oauth.Provider(r.FormValue("provider"))
	if oauth.ConfigFor(provider) == nil {
		provider = oauth.ProviderCodex
	}
	tenantID := r.FormValue("tenant_id")
	if tenantID == "" {
		tenantID = "default"
	}
	note := r.FormValue("note")
	p, authURL, err := s.oauth.Start(provider, tenantID, note)
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
	authURL, _ := url.QueryUnescape(r.URL.Query().Get("u"))
	if authURL == "" {
		authURL = "(start a new enrollment to get a fresh URL)"
	}
	cfg := oauth.ConfigFor(p.Provider)
	s.render(w, r, "oauth_show.html", map[string]any{
		"Active":  "accounts",
		"Title":   s.t(r, "enroll.title"),
		"Pending": p,
		"AuthURL": authURL,
		"Cfg":     cfg,
	})
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

var _ = json.Marshal
var _ = errors.New
