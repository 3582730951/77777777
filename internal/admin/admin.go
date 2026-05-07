// Package admin serves the Apple-styled web management UI plus the JSON API
// powering it. The UI is server-rendered HTML with HTMX for partial refresh.
// All assets are go:embedded so the binary stays self-contained.
package admin

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/enrollment"
	"github.com/llm-pool/gateway/internal/i18n"
	"github.com/llm-pool/gateway/internal/oauth"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

// alias so we can refer to the OAuth manager type without naming collisions
// (handlers_oauth.go defines persistence helpers).
type oauthManager = oauth.Manager

//go:embed templates/*.html static/*
var assets embed.FS

type Deps struct {
	Cfg          *config.Root
	Store        *store.Store
	Sched        *scheduler.Scheduler
	Logger       *slog.Logger
	ProbeFunc    func(ctx context.Context, accountID string) error
	DiscoverFunc func(ctx context.Context, accountID string) (*domain.QuotaState, error)
}

type Server struct {
	deps   Deps
	tpl    *template.Template
	crud   CrudDeps
	enroll *enrollment.Manager
	oauth  *oauthManager
}

// isEmpty returns true for nil, false bool, zero int, empty string, and empty slices/maps.
func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case bool:
		return !val
	case string:
		return val == ""
	case int:
		return val == 0
	case int64:
		return val == 0
	case []any:
		return len(val) == 0
	case []string:
		return len(val) == 0
	}
	// For slices/maps of concrete types ([]SlotView, []ProviderConfig, etc.) use reflection.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func New(d Deps) *Server {
	tplFS, err := fs.Sub(assets, "templates")
	if err != nil {
		d.Logger.Error("admin templates", "err", err)
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"hasPrefix": strings.HasPrefix,
		// truncate: called as {{.Value | truncate 12}} → truncate(12, .Value)
		// so first arg is the limit (int), second is the string.
		"truncate": func(n int, s string) string {
			if len(s) > n {
				return s[:n] + "…"
			}
			return s
		},
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
		"T": func(lang i18n.Lang, key string) string {
			return i18n.T(lang, key)
		},
		"langCN": func() i18n.Lang { return i18n.CN },
		"langEN": func() i18n.Lang { return i18n.EN },
		"div": func(a, b int64) int64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"gt64": func(a, b int64) bool { return a > b },
		"sub64": func(a, b int64) int64 {
			r := a - b
			if r < 0 {
				return 0
			}
			return r
		},
		"gtf": func(a, b float64) bool { return a > b },
		"subf": func(a, b float64) float64 {
			r := a - b
			if r < 0 {
				return 0
			}
			return r
		},
		"fmtf": func(v float64) string {
			if v == float64(int64(v)) {
				return fmt.Sprintf("%.0f", v)
			}
			return fmt.Sprintf("%.1f", v)
		},
		"fmtTokens": func(n int) string {
			v := float64(n)
			switch {
			case v >= 1e15:
				return fmt.Sprintf("%.2fP", v/1e15)
			case v >= 1e12:
				return fmt.Sprintf("%.2fT", v/1e12)
			case v >= 1e9:
				return fmt.Sprintf("%.2fG", v/1e9)
			case v >= 1e6:
				return fmt.Sprintf("%.2fM", v/1e6)
			case v >= 1e3:
				return fmt.Sprintf("%.1fK", v/1e3)
			default:
				return fmt.Sprintf("%d", n)
			}
		},
		// percent: accepts any numeric types via interface for template flexibility.
		"percent": func(used, limit interface{}) int64 {
			toI64 := func(v interface{}) int64 {
				switch x := v.(type) {
				case int:
					return int64(x)
				case int64:
					return x
				case int32:
					return int64(x)
				case float64:
					return int64(x)
				}
				return 0
			}
			u, l := toI64(used), toI64(limit)
			if l <= 0 {
				return 0
			}
			p := u * 100 / l
			if p > 100 {
				return 100
			}
			return p
		},
		"join": strings.Join,
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				if k, ok := pairs[i].(string); ok {
					m[k] = pairs[i+1]
				}
			}
			return m
		},
		// "not" / "empty": check if value is nil, false, 0, "", or empty slice.
		// We use interface{} to avoid template type errors on slices.
		"not": func(v any) bool {
			return isEmpty(v)
		},
		"empty": func(v any) bool {
			return isEmpty(v)
		},
		"sub": func(a, b int) int { return a - b },
		"add": func(a, b int) int { return a + b },
		"gt":  func(a, b int) bool { return a > b },
		"confColor": func(c string) string {
			switch c {
			case "confirmed_available", "likely_available":
				return "var(--ok)"
			case "suspected_issue", "cooling":
				return "var(--warn)"
			case "probably_exhausted":
				return "var(--err)"
			default:
				return "var(--fg-muted)"
			}
		},
		"confLabel": func(c string) string {
			switch c {
			case "confirmed_available":
				return "已确认可用"
			case "likely_available":
				return "可能可用"
			case "suspected_issue":
				return "疑似异常"
			case "probably_exhausted":
				return "疑似耗尽"
			case "cooling":
				return "冷却中"
			default:
				return c
			}
		},
		"statusColor": func(category string) string {
			switch category {
			case scheduler.SlotStatusHealthy:
				return "var(--ok)"
			case scheduler.SlotStatusLowQuota:
				return "var(--warn)"
			case scheduler.SlotStatusNoQuota, scheduler.SlotStatusBanned:
				return "var(--err)"
			default:
				return "var(--fg-muted)"
			}
		},
		"statusBadgeClass": func(category string) string {
			switch category {
			case scheduler.SlotStatusHealthy:
				return "ok"
			case scheduler.SlotStatusLowQuota:
				return "warn"
			case scheduler.SlotStatusNoQuota, scheduler.SlotStatusBanned:
				return "err"
			default:
				return "gray"
			}
		},
		"latColor": func(lat float64) string {
			if lat > 2000 {
				return "var(--err)"
			}
			if lat > 800 {
				return "var(--warn)"
			}
			return "var(--ok)"
		},
		"quotaPct": func(used, limit float64) int {
			if limit <= 0 {
				return 0
			}
			p := int(used * 100 / limit)
			if p > 100 {
				return 100
			}
			return p
		},
		"quotaColor": func(used, limit float64) string {
			if limit <= 0 {
				return "var(--ok)"
			}
			p := used * 100 / limit
			if p > 80 {
				return "var(--err)"
			}
			if p > 50 {
				return "var(--warn)"
			}
			return "var(--ok)"
		},
		"remainPct": func(used, limit float64) int {
			if limit <= 0 {
				return 100
			}
			r := int((limit - used) * 100 / limit)
			if r < 0 {
				return 0
			}
			if r > 100 {
				return 100
			}
			return r
		},
		"remainColor": func(used, limit float64) string {
			if limit <= 0 {
				return "var(--ok)"
			}
			remain := (limit - used) * 100 / limit
			if remain < 20 {
				return "var(--err)"
			}
			if remain < 50 {
				return "var(--warn)"
			}
			return "var(--ok)"
		},
		"providers": func(slots []scheduler.SlotView) []string {
			seen := map[string]bool{}
			var out []string
			for _, s := range slots {
				if s.Provider != "" && !seen[s.Provider] {
					seen[s.Provider] = true
					out = append(out, s.Provider)
				}
			}
			return out
		},
		"limitSlice": func(s []string, n int) []string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
	}).ParseFS(tplFS, "*.html")
	if err != nil {
		d.Logger.Error("parse admin templates", "err", err)
	}
	cryptoRandWrapper = cryptorand.Read
	return &Server{deps: d, tpl: tpl}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	staticFS, _ := fs.Sub(assets, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", noCacheWrap(http.FileServer(http.FS(staticFS)))))
	r.Get("/set-lang", s.handleSetLang)
	// CORS-open enrollment receiver (called by bookmarklet from chatgpt.com)
	r.Post("/enroll/{id}", s.handleEnrollSubmit)
	r.Options("/enroll/{id}", s.handleEnrollSubmit)
	r.Get("/login", s.handleLoginPage)
	r.Post("/login", s.handleLogin)
	r.Get("/logout", s.handleLogout)
	r.Get("/accounts/oauth/callback/relay", s.handleOAuthRelayCallback)

	// Tenant portal: username + password login.
	r.Get("/portal/login", s.handlePortalLoginPage)
	r.Post("/portal/login", s.handlePortalLoginV2)
	r.Get("/portal/logout", s.handlePortalLogout)
	r.Get("/portal/register", s.handlePortalRegisterPage)
	r.Post("/portal/register", s.handlePortalRegister)
	r.Group(func(r chi.Router) {
		r.Use(s.tenantAuthMiddleware)
		r.Get("/portal", s.handlePortalDashboard)
		r.Get("/portal/keys", s.handlePortalKeys)
		r.Post("/portal/keys", s.handlePortalCreateKey)
		r.Post("/portal/keys/{value}/revoke", s.handlePortalRevokeKey)
		r.Get("/portal/usage", s.handlePortalUsage)
		r.Get("/portal/guide", s.handlePortalGuide)
		// Portal-scoped JSON API (tenant session, auto-pinned to tenant_id).
		r.Get("/api/portal/cache-hit", s.handlePortalCacheHit)
		r.Get("/api/portal/cache-hit-series", s.handlePortalCacheHitSeries)
		r.Get("/api/portal/cache-hit-by-key", s.handlePortalCacheHitByKey)
		r.Get("/api/portal/charts/requests", s.handlePortalChartRequests)
	})

	// Admin UI
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/", s.handleDashboard)
		r.Get("/accounts", s.handleAccounts)
		r.Get("/accounts/new", s.handleAccountForm)
		r.Post("/accounts/new", s.handleAccountCreatePost)
		r.Get("/accounts/enroll", s.handleAccountEnrollForm)
		r.Post("/accounts/enroll", s.handleAccountEnrollCreate)
		r.Get("/accounts/enroll/{id}", s.handleAccountEnrollShow)
		r.Get("/accounts/enroll/{id}/status", s.handleAccountEnrollStatus)
		// OAuth flow
		r.Get("/accounts/oauth", s.handleOAuthStartGet)
		r.Post("/accounts/oauth", s.handleOAuthStartPost)
		r.Get("/accounts/oauth/{id}", s.handleOAuthShow)
		r.Get("/accounts/oauth/{id}/status", s.handleOAuthStatus)
		r.Post("/accounts/oauth/{id}/paste", s.handleOAuthPasteCallback)
		r.Get("/accounts/{id}", s.handleAccountDetail)
		r.Get("/accounts/{id}/edit", s.handleAccountEditForm)
		r.Post("/accounts/{id}/edit", s.handleAccountEditPost)
		r.Post("/accounts/{id}/probe", s.handleAccountProbeForm)
		r.Post("/accounts/{id}/delete", s.handleAccountDeleteForm)
		// Groups
		r.Get("/groups", s.handleGroups)
		r.Get("/groups/new", s.handleGroupForm)
		r.Post("/groups/new", s.handleGroupCreatePost)
		r.Get("/groups/{id}/edit", s.handleGroupEditForm)
		r.Post("/groups/{id}/edit", s.handleGroupEditPost)
		r.Post("/groups/{id}/delete", s.handleGroupDeleteForm)
		// Tenants
		r.Get("/tenants", s.handleTenants)
		r.Get("/tenants/new", s.handleTenantForm)
		r.Post("/tenants/new", s.handleTenantCreatePost)
		r.Post("/tenants/{id}/delete", s.handleTenantDeleteForm)
		r.Post("/tenants/{id}/rotate", s.handleTenantRotateForm)
		r.Post("/tenants/{id}/users", s.handleTenantUserCreatePost)
		r.Post("/tenants/{id}/users/{username}/reset", s.handleTenantUserResetPost)
		r.Post("/tenants/{id}/users/{username}/delete", s.handleTenantUserDeletePost)
		// API Keys
		r.Get("/keys", s.handleKeys)
		r.Get("/keys/new", s.handleKeyForm)
		r.Post("/keys/new", s.handleKeyCreatePost)
		r.Post("/keys/{value}/revoke", s.handleKeyRevokeForm)
		r.Get("/cluster", s.handleCluster)
		r.Get("/audit", s.handleAuditPage)
		r.Get("/guide", s.handleAdminGuide)
		// AutoReg SPA — serves the React frontend under /autoreg/*
		r.Handle("/autoreg", s.autoregSPAHandler())
		r.Handle("/autoreg/*", s.autoregSPAHandler())
		// AutoReg API proxy: /api/autoreg/X → /api/X on the Python service
		r.Handle("/api/autoreg/*", s.autoregProxyHandler())
		// Kiro Gateway management page
		r.Get("/kiro-gateway", s.handleKiroGateway)
	})

	// Mount JSON API + cluster peer endpoints (handlers_crud.go).
	s.mountCrud(r)
	return r
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("llm_pool_session")
	if c != nil {
		sessionStore.del(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "llm_pool_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", map[string]any{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	hash, err := s.deps.Store.GetAdminUser(r.Context(), user)
	if err != nil || hash == "" {
		s.render(w, r, "login.html", map[string]any{"Error": "用户或密码错误"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)); err != nil {
		s.render(w, r, "login.html", map[string]any{"Error": "用户或密码错误"})
		return
	}
	tok := generateToken()
	sessionStore.set(tok, user)
	http.SetCookie(w, &http.Cookie{
		Name: "llm_pool_session", Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 8 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("llm_pool_session")
		if err != nil || c.Value == "" || !sessionStore.valid(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	slots := s.deps.Sched.Snapshot()
	healthy := 0
	for _, sl := range slots {
		if sl.Healthy {
			healthy++
		}
	}
	s.render(w, r, "dashboard.html", map[string]any{
		"Active":  "dashboard",
		"Title":   "概览",
		"Slots":   slots,
		"Healthy": healthy,
		"Total":   len(slots),
		"Cfg":     s.deps.Cfg,
	})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	// Merge DB accounts + scheduler runtime state (same logic as JSON API).
	// This ensures accounts in DB but not yet registered in scheduler still appear.
	slots := s.deps.Sched.Snapshot()
	slotByID := map[string]scheduler.SlotView{}
	for _, sl := range slots {
		slotByID[sl.AccountID] = sl
	}
	accs, _ := s.deps.Store.ListAccounts(r.Context(), "")
	seen := map[string]bool{}
	var merged []scheduler.SlotView
	for _, a := range accs {
		seen[a.ID] = true
		if sl, ok := slotByID[a.ID]; ok {
			sl.Email = a.Email
			if sl.PlanTier == "" {
				sl.PlanTier = a.PlanTier
			}
			merged = append(merged, sl)
		} else {
			merged = append(merged, scheduler.SlotViewFromAccount(a))
		}
	}
	for _, sl := range slots {
		if !seen[sl.AccountID] {
			merged = append(merged, sl)
		}
	}
	scheduler.SortSlotViewsForPick(merged)
	s.render(w, r, "accounts.html", map[string]any{
		"Active": "accounts",
		"Title":  "账号池",
		"Slots":  merged,
	})
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	dynGroups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	s.render(w, r, "groups.html", map[string]any{
		"Active": "groups",
		"Title":  "分组与路由",
		"Cfg":    s.deps.Cfg,
		"Groups": dynGroups,
	})
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	type peerView struct {
		Name      string
		URL       string
		OK        bool
		LatencyMS int64
		Snapshot  map[string]any
	}
	views := []peerView{}
	if s.deps.Cfg.Cluster.Enabled {
		client := &http.Client{Timeout: s.deps.Cfg.Cluster.PullTimeout}
		for _, p := range s.deps.Cfg.Cluster.Peers {
			pv := peerView{Name: p.Name, URL: p.URL}
			start := time.Now()
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(p.URL, "/")+"/api/cluster/snapshot", nil)
			req.Header.Set("X-Pool-Token", p.Token)
			resp, err := client.Do(req)
			pv.LatencyMS = time.Since(start).Milliseconds()
			if err == nil && resp != nil {
				if resp.StatusCode == 200 {
					pv.OK = true
					_ = json.NewDecoder(resp.Body).Decode(&pv.Snapshot)
				}
				resp.Body.Close()
			}
			views = append(views, pv)
		}
	}
	s.render(w, r, "cluster.html", map[string]any{
		"Active": "cluster",
		"Title":  "集群",
		"Peers":  views,
		"Cfg":    s.deps.Cfg,
	})
}

func (s *Server) handleAPIAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.deps.Sched.Snapshot())
}

func (s *Server) handleAPISnapshot(w http.ResponseWriter, r *http.Request) {
	type snap struct {
		Identity string               `json:"identity"`
		Region   string               `json:"region"`
		Time     time.Time            `json:"time"`
		Slots    []scheduler.SlotView `json:"slots"`
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap{
		Identity: s.deps.Cfg.Cluster.Identity,
		Region:   s.deps.Cfg.Cluster.Region,
		Time:     time.Now(),
		Slots:    s.deps.Sched.Snapshot(),
	})
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if s.tpl == nil {
		http.Error(w, "templates not loaded", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["Lang"]; !ok {
		data["Lang"] = langFromReq(r)
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[render] template %s error: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func noCacheWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// minimal in-memory session store (good enough for single-instance MVP)

type sessionMap struct {
	m map[string]session
}
type session struct {
	user      string
	createdAt time.Time
}

var sessionStore = &sessionMap{m: map[string]session{}}

func (s *sessionMap) set(token, user string) {
	s.m[token] = session{user: user, createdAt: time.Now()}
}
func (s *sessionMap) valid(token string) bool {
	v, ok := s.m[token]
	if !ok {
		return false
	}
	if time.Since(v.createdAt) > 12*time.Hour {
		delete(s.m, token)
		return false
	}
	return true
}
func (s *sessionMap) del(token string) {
	delete(s.m, token)
}

func generateToken() string {
	b := make([]byte, 32)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

// Just to avoid an "unused import" if the project disables crypto features.
var _ = errors.New
var _ = context.Background
