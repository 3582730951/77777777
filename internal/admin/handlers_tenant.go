package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/store"
)

// Tenant portal: separate session cookie + master-key auth.

const tenantSessionCookie = "llm_pool_tenant_session"

type tenantSessionStoreT struct {
	m map[string]tenantSession
}
type tenantSession struct {
	tenantID  string
	createdAt time.Time
}

var tenantSessionStore = &tenantSessionStoreT{m: map[string]tenantSession{}}

func (t *tenantSessionStoreT) set(token, tenantID string) {
	t.m[token] = tenantSession{tenantID: tenantID, createdAt: time.Now()}
}
func (t *tenantSessionStoreT) take(token string) (string, bool) {
	v, ok := t.m[token]
	if !ok {
		return "", false
	}
	if time.Since(v.createdAt) > 12*time.Hour {
		delete(t.m, token)
		return "", false
	}
	return v.tenantID, true
}
func (t *tenantSessionStoreT) del(token string) { delete(t.m, token) }

type tenantCtxKey struct{}

func (s *Server) tenantAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(tenantSessionCookie)
		if err != nil || c.Value == "" {
			http.Redirect(w, r, "/portal/login", http.StatusSeeOther)
			return
		}
		tid, ok := tenantSessionStore.take(c.Value)
		if !ok {
			http.Redirect(w, r, "/portal/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), tenantCtxKey{}, tid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tenantFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(tenantCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func (s *Server) handlePortalLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "portal_login.html", map[string]any{})
}

func (s *Server) handlePortalLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	mk := strings.TrimSpace(r.FormValue("master_key"))
	if mk == "" {
		s.render(w, r, "portal_login.html", map[string]any{"Error": "请输入租户主密钥"})
		return
	}
	tid, ok, _ := s.deps.Store.VerifyTenantMasterKey(r.Context(), mk)
	if !ok {
		s.render(w, r, "portal_login.html", map[string]any{"Error": "主密钥无效"})
		return
	}
	tok := generateToken()
	tenantSessionStore.set(tok, tid)
	http.SetCookie(w, &http.Cookie{
		Name: tenantSessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	http.Redirect(w, r, "/portal", http.StatusSeeOther)
}

func (s *Server) handlePortalLogout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(tenantSessionCookie)
	if c != nil {
		tenantSessionStore.del(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: tenantSessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/portal/login", http.StatusSeeOther)
}

func (s *Server) handlePortalDashboard(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), tid)
	keys, _ := s.deps.Store.ListAPIKeys(r.Context(), tid, "")

	// Count today's requests for this tenant
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	requestsToday := 0
	tokensToday := 0
	if logs, err := s.deps.Store.QueryAuditByTenant(r.Context(), tid, 1000); err == nil {
		for _, l := range logs {
			if l.At.After(todayStart) {
				requestsToday++
				tokensToday += l.InputTokens + l.OutputTokens
			}
		}
	}

	gatewayURL := s.gatewayURL(r)

	s.render(w, r, "portal_dashboard.html", map[string]any{
		"PActive":       "dashboard",
		"TenantID":      tid,
		"Groups":        groups,
		"Keys":          keys,
		"KeyCount":      len(keys),
		"GroupCount":    len(groups),
		"RequestsToday": requestsToday,
		"TokensToday":   tokensToday,
		"GatewayURL":    gatewayURL,
	})
}

func (s *Server) handlePortalKeys(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	// Include own-tenant groups + default/global groups (cyber groups live in "default")
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), tid)
	if tid != "default" {
		globalGroups, _ := s.deps.Store.ListDynGroups(r.Context(), "default")
		groups = append(groups, globalGroups...)
	}
	keys, _ := s.deps.Store.ListAPIKeys(r.Context(), tid, "")
	flashKey := r.URL.Query().Get("flash")
	flashVal := ""
	if flashKey != "" {
		flashVal = keyFlashStore.take(flashKey)
	}
	s.render(w, r, "portal_keys.html", map[string]any{
		"PActive":    "keys",
		"TenantID":   tid,
		"Groups":     groups,
		"Keys":       keys,
		"FlashKey":   flashVal,
		"GatewayURL": s.gatewayURL(r),
	})
}

func (s *Server) handlePortalCreateKey(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	groupID := r.FormValue("group_id")
	label := r.FormValue("label")
	if groupID == "" {
		http.Error(w, "group_id required", 400)
		return
	}
	// guard: group must belong to this tenant OR be a global "default" group
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), tid)
	globalGroups, _ := s.deps.Store.ListDynGroups(r.Context(), "default")
	allowed := false
	for _, g := range append(groups, globalGroups...) {
		if g.ID == groupID {
			allowed = true
			break
		}
	}
	if !allowed {
		for _, g := range s.deps.Cfg.Groups {
			if g.ID == groupID && (g.TenantID == tid || g.TenantID == "default") {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		http.Error(w, "group not in tenant", 403)
		return
	}
	rec, err := s.deps.Store.CreateAPIKey(r.Context(), tid, groupID, label)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "apikey", "", groupID, "tenant created key: "+rec.Label)
	}
	keyFlashStore.set(rec.Value, rec.Value)
	http.Redirect(w, r, "/portal/keys?flash="+rec.Value, http.StatusSeeOther)
}

func (s *Server) handlePortalRevokeKey(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	val := chiURLParam(r, "value")
	rec, _ := s.deps.Store.ListAPIKeys(r.Context(), tid, "")
	allowed := false
	for _, k := range rec {
		if k.Value == val {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "key not in tenant", 403)
		return
	}
	_ = s.deps.Store.RevokeAPIKey(r.Context(), val)
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	http.Redirect(w, r, "/portal/keys", http.StatusSeeOther)
}

func (s *Server) handlePortalUsage(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	logs, _ := s.deps.Store.QueryAuditByTenant(r.Context(), tid, 200)

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	requestsTotal := len(logs)
	requestsToday := 0
	tokensTotal := 0
	for _, l := range logs {
		tokensTotal += l.InputTokens + l.OutputTokens
		if l.At.After(todayStart) {
			requestsToday++
		}
	}

	// Build last-7-day daily buckets for bar chart
	type dayStat struct {
		Label string
		Count int
	}
	days := make([]dayStat, 7)
	for i := range days {
		d := now.AddDate(0, 0, -(6 - i))
		days[i] = dayStat{Label: d.Format("01/02")}
	}
	for _, l := range logs {
		diff := int(todayStart.Sub(time.Date(l.At.Year(), l.At.Month(), l.At.Day(), 0, 0, 0, 0, now.Location())).Hours() / 24)
		idx := 6 - diff
		if idx >= 0 && idx < 7 {
			days[idx].Count++
		}
	}
	maxReqs := 0
	for _, d := range days {
		if d.Count > maxReqs {
			maxReqs = d.Count
		}
	}

	s.render(w, r, "portal_usage.html", map[string]any{
		"PActive":       "usage",
		"TenantID":      tid,
		"Logs":          logs,
		"RequestsTotal": requestsTotal,
		"RequestsToday": requestsToday,
		"TokensTotal":   tokensTotal,
		"DailyStats":    days,
		"MaxDailyReqs":  maxReqs,
	})
}

// chiURLParam without forcing chi import here to keep the file self-contained.
func chiURLParam(r *http.Request, name string) string {
	// Use chi via the request route context; reuse the chi.URLParam from form file.
	return chiParam(r, name)
}

var chiParam = func(r *http.Request, name string) string { return "" }

func (s *Server) gatewayURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := strings.Split(r.Host, ":")[0]
	gw := scheme + "://" + host
	if addr := s.deps.Cfg.Server.GatewayAddr; addr != "" {
		if strings.HasPrefix(addr, ":") {
			gw = scheme + "://" + host + addr
		}
	}
	return gw
}

// initialised in handlers_crud.go where chi is already imported.
var _ = store.APIKeyRecord{}
