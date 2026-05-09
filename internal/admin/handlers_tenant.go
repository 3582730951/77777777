package admin

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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

var publicIPCache = struct {
	sync.Mutex
	value   string
	expires time.Time
}{}

func (s *Server) gatewayURL(r *http.Request) string {
	for _, key := range []string{"LLM_POOL_GATEWAY_URL", "PUBLIC_GATEWAY_URL", "GATEWAY_PUBLIC_URL"} {
		if value := strings.TrimRight(strings.TrimSpace(os.Getenv(key)), "/"); value != "" {
			return value
		}
	}

	scheme := firstCSV(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		switch {
		case strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on"):
			scheme = "https"
		case strings.EqualFold(r.Header.Get("X-Forwarded-Scheme"), "https"):
			scheme = "https"
		case r.TLS != nil:
			scheme = "https"
		default:
			scheme = "http"
		}
	}

	host := firstCSV(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	host = hostWithoutPort(host)
	if shouldUsePublicIP(host) {
		if ip := configuredPublicHost(); ip != "" {
			host = ip
		} else if ip := discoverPublicIP(r.Context()); ip != "" {
			host = ip
		}
	}

	port := gatewayPort(s.deps.Cfg.Server.GatewayAddr)
	return scheme + "://" + formatURLHost(host, port, scheme)
}

func configuredPublicHost() string {
	for _, key := range []string{"LLM_POOL_PUBLIC_HOST", "PUBLIC_HOST", "PUBLIC_IP"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return hostWithoutPort(value)
		}
	}
	return ""
}

func discoverPublicIP(ctx context.Context) string {
	publicIPCache.Lock()
	if time.Now().Before(publicIPCache.expires) {
		value := publicIPCache.value
		publicIPCache.Unlock()
		return value
	}
	publicIPCache.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return cachePublicIP("", 5*time.Minute)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cachePublicIP("", 5*time.Minute)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(body))
	if parsed := net.ParseIP(ip); parsed == nil || !isPublicIP(parsed) {
		return cachePublicIP("", 5*time.Minute)
	}
	return cachePublicIP(ip, 30*time.Minute)
}

func cachePublicIP(value string, ttl time.Duration) string {
	publicIPCache.Lock()
	publicIPCache.value = value
	publicIPCache.expires = time.Now().Add(ttl)
	publicIPCache.Unlock()
	return value
}

func gatewayPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		return strings.TrimPrefix(addr, ":")
	}
	_, port, err := net.SplitHostPort(addr)
	if err == nil {
		return port
	}
	if _, err := strconv.Atoi(addr); err == nil {
		return addr
	}
	return ""
}

func hostWithoutPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		if idx := strings.Index(host, "://"); idx >= 0 {
			host = host[idx+3:]
		}
	}
	host = strings.TrimSuffix(host, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(host, "[]")
}

func formatURLHost(host, port, scheme string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

func shouldUsePublicIP(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return !isPublicIP(ip)
}

func isPublicIP(ip net.IP) bool {
	return ip != nil &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast()
}

// initialised in handlers_crud.go where chi is already imported.
var _ = store.APIKeyRecord{}
