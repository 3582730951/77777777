package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	windsurfPkg "github.com/llm-pool/gateway/internal/provider/windsurf"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

// Extended Deps used by the CRUD layer; superset of original Deps.
type CrudDeps struct {
	Resolver *auth.DynamicResolver
	Audit    *audit.Logger
}

func (s *Server) WithCrud(d CrudDeps) *Server {
	s.crud = d
	return s
}

// mountCrud mounts every JSON CRUD endpoint behind /api/admin/* (admin login
// required) and /api/cluster/* (peer-to-peer token auth, see cluster.go).
func (s *Server) mountCrud(r chi.Router) {
	if s.crud.Resolver == nil {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware) // admin session cookie

		// Tenants
		r.Get("/api/admin/tenants", s.handleListTenants)
		r.Post("/api/admin/tenants", s.handleCreateTenant)
		r.Patch("/api/admin/tenants/{id}", s.handleUpdateTenant)
		r.Delete("/api/admin/tenants/{id}", s.handleDeleteTenant)
		r.Post("/api/admin/tenants/{id}/master-key", s.handleRotateTenantKey)

		// Groups
		r.Get("/api/admin/groups", s.handleListGroups)
		r.Post("/api/admin/groups", s.handleUpsertGroup)
		r.Put("/api/admin/groups/{id}", s.handleUpsertGroupID)
		r.Delete("/api/admin/groups/{id}", s.handleDeleteGroup)

		// Accounts
		r.Get("/api/admin/accounts", s.handleListAccountsAPI)
		r.Get("/api/admin/accounts/export", s.handleExportAccountsAPI)
		r.Post("/api/admin/accounts", s.handleCreateAccount)
		r.Patch("/api/admin/accounts/{id}", s.handleUpdateAccount)
		r.Delete("/api/admin/accounts/{id}", s.handleDeleteAccount)
		r.Post("/api/admin/accounts/{id}/probe", s.handleProbeAccount)
		r.Post("/api/admin/accounts/{id}/discover", s.handleDiscoverAccount)
		r.Get("/api/admin/accounts/{id}/series", s.handleAccountSeries)

		// Remote chat
		r.Get("/api/admin/remote-chat/config", s.handleGetRemoteChatConfig)
		r.Put("/api/admin/remote-chat/config", s.handleSetRemoteChatConfig)
		r.Delete("/api/admin/remote-chat/config", s.handleClearRemoteChatConfig)

		// API Keys
		r.Get("/api/admin/api-keys", s.handleListAPIKeys)
		r.Post("/api/admin/api-keys", s.handleCreateAPIKey)
		r.Delete("/api/admin/api-keys/{value}", s.handleRevokeAPIKey)

		// Audit / charts
		r.Get("/api/admin/audit", s.handleAuditQuery)
		r.Get("/api/admin/audit/stream", s.handleAuditStream)
		r.Get("/api/admin/charts/requests", s.handleChartRequests)
		r.Get("/api/admin/charts/accounts", s.handleChartAccounts)
		r.Get("/api/admin/charts/cache-hit", s.handleCacheHitOverall)
		r.Get("/api/admin/charts/cache-hit-series", s.handleCacheHitSeries)
		r.Get("/api/admin/charts/cache-hit-by-key", s.handleCacheHitByKey)
		r.Get("/api/admin/charts/provider-breakdown", s.handleProviderBreakdown)
		r.Get("/api/admin/charts/token-trend", s.handleTokenTrend)
		r.Get("/api/admin/system", s.handleSystemInfo)
		r.Get("/api/admin/backup", s.handleBackup)
		r.Post("/api/admin/cluster/push", s.handleClusterPush)
		r.Post("/api/admin/accounts/bulk-import", s.handleAccountsBulkImport)
		r.Post("/api/admin/windsurf/enroll", s.handleWindsurfEnroll)
		r.Post("/api/admin/kiro/enroll", s.handleKiroEnroll)
	})

	// Cluster federation peer endpoints (token-auth, separate from admin login).
	r.Get("/api/cluster/snapshot", s.handlePeerSnapshot)
	r.Get("/api/cluster/accounts/summary", s.handlePeerAccountsSummary)
	r.Post("/api/cluster/config/accept", s.handleAcceptClusterConfig)
}

// ----- helpers -----

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSONStatus(w, status, map[string]any{"error": msg})
}

// ----- Tenants -----

func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.deps.Store.ListDynTenants(r.Context())
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type tenantView struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
		Source    string    `json:"source"`
	}
	out := []tenantView{}
	seen := map[string]bool{}
	for _, t := range ts {
		out = append(out, tenantView{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, Source: "db"})
		seen[t.ID] = true
	}
	for _, t := range s.deps.Cfg.Tenants {
		if seen[t.ID] {
			continue
		}
		out = append(out, tenantView{ID: t.ID, Name: t.Name, Source: "yaml"})
	}
	writeJSONStatus(w, 200, out)
}

type createTenantReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.ID == "" {
		errJSON(w, 400, "id required")
		return
	}
	if req.Name == "" {
		req.Name = req.ID
	}
	if err := s.deps.Store.UpsertDynTenant(r.Context(), store.Tenant{ID: req.ID, Name: req.Name, CreatedAt: time.Now()}); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	plain, err := s.deps.Store.SetTenantMasterKey(r.Context(), req.ID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("info", "tenant", "", "", "tenant created: "+req.ID)
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 201, map[string]any{"id": req.ID, "name": req.Name, "master_key": plain})
}

func (s *Server) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req createTenantReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Name == "" {
		errJSON(w, 400, "name required")
		return
	}
	if err := s.deps.Store.UpsertDynTenant(r.Context(), store.Tenant{ID: id, Name: req.Name, CreatedAt: time.Now()}); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 200, map[string]any{"id": id, "name": req.Name})
}

func (s *Server) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.deps.Store.DeleteDynTenant(r.Context(), id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("warn", "tenant", "", "", "tenant deleted: "+id)
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 200, map[string]any{"deleted": id})
}

func (s *Server) handleRotateTenantKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	plain, err := s.deps.Store.SetTenantMasterKey(r.Context(), id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("warn", "tenant", "", "", "master key rotated: "+id)
	writeJSONStatus(w, 200, map[string]any{"id": id, "master_key": plain})
}

// ----- Groups -----

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant")
	dyn, err := s.deps.Store.ListDynGroups(r.Context(), tenantID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type out struct {
		ID                    string            `json:"id"`
		TenantID              string            `json:"tenant_id"`
		Provider              string            `json:"provider"`
		Models                []string          `json:"models"`
		ModelAliases          map[string]string `json:"model_aliases"`
		ModelWhitelist        []string          `json:"model_whitelist"`
		AccountIDs            []string          `json:"account_ids"`
		SystemPrompt          string            `json:"system_prompt"`
		SystemPromptMode      string            `json:"system_prompt_mode"`
		SystemPromptInjection string            `json:"system_prompt_injection"`
		Source                string            `json:"source"`
	}
	res := []out{}
	seen := map[string]bool{}
	for _, g := range dyn {
		seen[g.ID] = true
		res = append(res, out{
			ID: g.ID, TenantID: g.TenantID, Provider: g.Provider,
			Models: g.Models, ModelAliases: g.ModelAliases,
			ModelWhitelist: g.ModelWhitelist, AccountIDs: g.AccountIDs,
			SystemPrompt: g.SystemPrompt, SystemPromptMode: g.SystemPromptMode,
			SystemPromptInjection: g.SystemPromptInjection, Source: "db",
		})
	}
	for _, g := range s.deps.Cfg.Groups {
		if seen[g.ID] {
			continue
		}
		if tenantID != "" && g.TenantID != tenantID {
			continue
		}
		res = append(res, out{
			ID: g.ID, TenantID: g.TenantID, Provider: g.Provider,
			Models: g.Models, ModelAliases: g.ModelAliases,
			ModelWhitelist: g.ModelWhitelist, AccountIDs: g.AccountIDs,
			SystemPrompt: g.SystemPrompt, SystemPromptMode: g.SystemPromptMode,
			SystemPromptInjection: g.SystemPromptInjection, Source: "yaml",
		})
	}
	writeJSONStatus(w, 200, res)
}

type upsertGroupReq struct {
	ID                    string            `json:"id"`
	TenantID              string            `json:"tenant_id"`
	Provider              string            `json:"provider"`
	Models                []string          `json:"models"`
	ModelAliases          map[string]string `json:"model_aliases"`
	ModelWhitelist        []string          `json:"model_whitelist"`
	AccountIDs            []string          `json:"account_ids"`
	SystemPrompt          string            `json:"system_prompt"`
	SystemPromptMode      string            `json:"system_prompt_mode"`
	SystemPromptInjection string            `json:"system_prompt_injection"`
}

func (s *Server) handleUpsertGroup(w http.ResponseWriter, r *http.Request) {
	var req upsertGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.ID == "" || req.TenantID == "" || req.Provider == "" {
		errJSON(w, 400, "id, tenant_id, provider required")
		return
	}
	g := store.DynGroup{
		ID: req.ID, TenantID: req.TenantID, Provider: req.Provider,
		Models: req.Models, ModelAliases: req.ModelAliases,
		ModelWhitelist: req.ModelWhitelist, AccountIDs: req.AccountIDs,
		SystemPrompt: req.SystemPrompt, SystemPromptMode: req.SystemPromptMode,
		SystemPromptInjection: req.SystemPromptInjection,
		CreatedAt:             time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.deps.Store.UpsertDynGroup(r.Context(), g); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("info", "group", "", req.ID, "group upserted")
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 201, req)
}

func (s *Server) handleUpsertGroupID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req upsertGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	req.ID = id
	if req.TenantID == "" {
		errJSON(w, 400, "tenant_id required")
		return
	}
	g := store.DynGroup{
		ID: id, TenantID: req.TenantID, Provider: req.Provider,
		Models: req.Models, ModelAliases: req.ModelAliases,
		ModelWhitelist: req.ModelWhitelist, AccountIDs: req.AccountIDs,
		SystemPrompt: req.SystemPrompt, SystemPromptMode: req.SystemPromptMode,
		SystemPromptInjection: req.SystemPromptInjection,
		UpdatedAt:             time.Now(),
	}
	if err := s.deps.Store.UpsertDynGroup(r.Context(), g); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 200, req)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.deps.Store.DeleteDynGroup(r.Context(), id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("warn", "group", "", id, "group deleted")
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 200, map[string]any{"deleted": id})
}

// ----- Accounts -----

type accountView struct {
	ID             string                   `json:"id"`
	TenantID       string                   `json:"tenant_id"`
	Provider       string                   `json:"provider"`
	Email          string                   `json:"email,omitempty"`
	State          string                   `json:"state"`
	PlanTier       string                   `json:"plan_tier"`
	StealthProfile string                   `json:"stealth_profile"`
	UA             string                   `json:"ua"`
	Proxy          string                   `json:"proxy"`
	Confidence     string                   `json:"confidence"`
	Healthy        bool                     `json:"healthy"`
	StatusCategory string                   `json:"status_category"`
	StatusLabel    string                   `json:"status_label"`
	SortRank       int                      `json:"sort_rank"`
	PickCost       float64                  `json:"pick_cost"`
	EWMAMs         float64                  `json:"ewma_latency"`
	Inflight       int                      `json:"inflight"`
	BreakerOpen    bool                     `json:"breaker_open"`
	OpenUntil      *time.Time               `json:"open_until,omitempty"`
	LastSuccess    *time.Time               `json:"last_success,omitempty"`
	LastFailure    *time.Time               `json:"last_failure,omitempty"`
	Models         []domain.ModelCapability `json:"models,omitempty"`
	// Quota fields for dashboard visualization.
	QuotaShortUsed   float64    `json:"quota_short_used"`
	QuotaShortLimit  float64    `json:"quota_short_limit"`
	QuotaShortReset  *time.Time `json:"quota_short_reset,omitempty"`
	QuotaLongUsed    float64    `json:"quota_long_used"`
	QuotaLongLimit   float64    `json:"quota_long_limit"`
	QuotaLongReset   *time.Time `json:"quota_long_reset,omitempty"`
	DiscoveredModels []string   `json:"discovered_models,omitempty"`
}

func (s *Server) handleListAccountsAPI(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant")
	views := []accountView{}
	slots := s.deps.Sched.Snapshot()
	slotByID := map[string]scheduler.SlotView{}
	for _, sl := range slots {
		slotByID[sl.AccountID] = sl
	}
	accs, err := s.deps.Store.ListAccounts(r.Context(), tenantID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	for _, a := range accs {
		v := accountView{
			ID: a.ID, TenantID: a.TenantID, Provider: a.Provider,
			Email: a.Email, State: string(a.State), PlanTier: a.PlanTier,
			StealthProfile: a.StealthProfile, UA: a.UA, Proxy: a.Proxy,
			Models: a.Quota.DiscoveredModels,
		}
		if sl, ok := slotByID[a.ID]; ok {
			applySlotToAccountView(&v, sl)
		} else {
			applySlotToAccountView(&v, scheduler.SlotViewFromAccount(a))
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].SortRank != views[j].SortRank {
			return views[i].SortRank < views[j].SortRank
		}
		if views[i].PickCost != views[j].PickCost {
			return views[i].PickCost < views[j].PickCost
		}
		return views[i].ID < views[j].ID
	})
	writeJSONStatus(w, 200, views)
}

func applySlotToAccountView(v *accountView, sl scheduler.SlotView) {
	if sl.State != "" {
		v.State = sl.State
	}
	if sl.PlanTier != "" {
		v.PlanTier = sl.PlanTier
	}
	v.Confidence = sl.Confidence
	v.Healthy = sl.Healthy
	v.StatusCategory = sl.StatusCategory
	v.StatusLabel = sl.StatusLabel
	v.SortRank = sl.SortRank
	v.PickCost = sl.PickCost
	v.EWMAMs = sl.EWMALatency
	v.Inflight = sl.Inflight
	v.BreakerOpen = sl.BreakerState == int(domain.BreakerOpen)
	if !sl.OpenUntil.IsZero() {
		ou := sl.OpenUntil
		v.OpenUntil = &ou
	}
	if !sl.LastSuccess.IsZero() {
		ls := sl.LastSuccess
		v.LastSuccess = &ls
	}
	if !sl.LastFailure.IsZero() {
		lf := sl.LastFailure
		v.LastFailure = &lf
	}
	v.QuotaShortUsed = sl.QuotaShortUsed
	v.QuotaShortLimit = sl.QuotaShortLimit
	v.QuotaLongUsed = sl.QuotaLongUsed
	v.QuotaLongLimit = sl.QuotaLongLimit
	if !sl.QuotaShortReset.IsZero() {
		t := sl.QuotaShortReset
		v.QuotaShortReset = &t
	}
	if !sl.QuotaLongReset.IsZero() {
		t := sl.QuotaLongReset
		v.QuotaLongReset = &t
	}
	v.DiscoveredModels = sl.DiscoveredModels
}

type createAccountReq struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	Provider       string `json:"provider"`
	Email          string `json:"email"`
	PlanTier       string `json:"plan_tier"`
	StealthProfile string `json:"stealth_profile"`
	UA             string `json:"ua"`
	Proxy          string `json:"proxy"`
	SessionToken   string `json:"session_token"`
	RefreshToken   string `json:"refresh_token"`
	Cookies        string `json:"cookies"` // raw netscape format or JSON
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.ID == "" || req.Provider == "" {
		errJSON(w, 400, "id, provider required")
		return
	}
	if req.TenantID == "" {
		req.TenantID = "default"
	}
	if req.StealthProfile == "" {
		req.StealthProfile = "chrome_124_windows"
	}
	a := &domain.Account{
		ID:             req.ID,
		TenantID:       req.TenantID,
		Provider:       req.Provider,
		Email:          accountEmailFromImport(req.Email, req.SessionToken),
		PlanTier:       req.PlanTier,
		StealthProfile: req.StealthProfile,
		UA:             req.UA,
		Proxy:          req.Proxy,
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		Cookies:      []byte(req.Cookies),
		SessionToken: req.SessionToken,
		RefreshToken: req.RefreshToken,
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Register(a)
	s.crud.Audit.Log("info", "account", req.ID, "", "account created")
	writeJSONStatus(w, 201, accountView{
		ID: a.ID, TenantID: a.TenantID, Provider: a.Provider, Email: a.Email,
		State: string(a.State), PlanTier: a.PlanTier,
		StealthProfile: a.StealthProfile, UA: a.UA, Proxy: a.Proxy,
	})
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.deps.Sched.AccountByID(id)
	if !ok {
		errJSON(w, 404, "not found")
		return
	}
	var req createAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.PlanTier != "" {
		a.PlanTier = req.PlanTier
	}
	if req.Email != "" || req.SessionToken != "" {
		a.Email = accountEmailFromImport(req.Email, req.SessionToken)
	}
	if req.UA != "" {
		a.UA = req.UA
	}
	if req.Proxy != "" {
		a.Proxy = req.Proxy
	}
	if req.StealthProfile != "" {
		a.StealthProfile = req.StealthProfile
	}
	a.UpdatedAt = time.Now()
	sec := store.AccountSecret{}
	if req.SessionToken != "" || req.Cookies != "" {
		sec = store.AccountSecret{
			Cookies:      []byte(req.Cookies),
			SessionToken: req.SessionToken,
			RefreshToken: req.RefreshToken,
		}
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Register(a)
	writeJSONStatus(w, 200, map[string]any{"updated": id})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.deps.Store.DeleteAccount(r.Context(), id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Unregister(id)
	s.crud.Audit.Log("warn", "account", id, "", "account deleted")
	writeJSONStatus(w, 200, map[string]any{"deleted": id})
}

func (s *Server) handleProbeAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.deps.ProbeFunc == nil {
		errJSON(w, 500, "probe func unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := s.deps.ProbeFunc(ctx, id)
	if err != nil {
		s.crud.Audit.Log("warn", "probe", id, "", "probe failed: "+err.Error())
		writeJSONStatus(w, 200, map[string]any{"ok": false, "err": err.Error()})
		return
	}
	s.deps.Sched.MarkSuccess(id, 100)
	// Also run discover to refresh quota after successful probe.
	if s.deps.DiscoverFunc != nil {
		if state, derr := s.deps.DiscoverFunc(ctx, id); derr == nil && state != nil {
			s.deps.Sched.UpdateQuota(id, state)
		}
	}
	s.crud.Audit.Log("info", "probe", id, "", "probe ok")
	writeJSONStatus(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDiscoverAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.deps.DiscoverFunc == nil {
		errJSON(w, 500, "discover func unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	state, err := s.deps.DiscoverFunc(ctx, id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	// Write discovered quota back into the scheduler slot so the dashboard reflects it.
	s.deps.Sched.UpdateQuota(id, state)
	s.crud.Audit.Log("info", "discover", id, "",
		fmt.Sprintf("models=%d 5h=%.0f%% 7d=%.0f%%", len(state.DiscoveredModels), state.ShortWindow.Used, state.LongWindow.Used))
	writeJSONStatus(w, 200, state)
}

func (s *Server) handleAccountSeries(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	windowStr := r.URL.Query().Get("window")
	dur := 6 * time.Hour
	if d, err := time.ParseDuration(windowStr); err == nil && d > 0 && d < 7*24*time.Hour {
		dur = d
	}
	since := time.Now().Add(-dur)
	samples, err := s.deps.Store.QueryQuotaSamples(r.Context(), id, since, 720)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, samples)
}

// ----- API Keys -----

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	group := r.URL.Query().Get("group")
	keys, err := s.deps.Store.ListAPIKeys(r.Context(), tenant, group)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, keys)
}

type createAPIKeyReq struct {
	TenantID string `json:"tenant_id"`
	GroupID  string `json:"group_id"`
	Label    string `json:"label"`
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req createAPIKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.TenantID == "" || req.GroupID == "" {
		errJSON(w, 400, "tenant_id, group_id required")
		return
	}
	rec, err := s.deps.Store.CreateAPIKey(r.Context(), req.TenantID, req.GroupID, req.Label)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("info", "apikey", "", req.GroupID, "key created: "+rec.Label)
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 201, rec)
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	val := chi.URLParam(r, "value")
	if err := s.deps.Store.RevokeAPIKey(r.Context(), val); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.crud.Audit.Log("warn", "apikey", "", "", "key revoked")
	_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	writeJSONStatus(w, 200, map[string]any{"revoked": val})
}

// ----- Audit + Charts -----

func (s *Server) handleAuditQuery(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 200
	}
	entries, err := s.deps.Store.QueryAudit(r.Context(), limit, 0)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, entries)
}

func (s *Server) handleAuditStream(w http.ResponseWriter, r *http.Request) {
	if s.crud.Audit == nil {
		errJSON(w, 500, "audit logger missing")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	ch, cancel := s.crud.Audit.Subscribe(64)
	defer cancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		case <-heartbeat.C:
			fmt.Fprintf(w, ":hb\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleChartRequests(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	winStr := r.URL.Query().Get("window")
	bucketsStr := r.URL.Query().Get("buckets")
	dur := 1 * time.Hour
	if d, err := time.ParseDuration(winStr); err == nil && d > 0 && d <= 7*24*time.Hour {
		dur = d
	}
	buckets := 60
	if n, err := strconv.Atoi(bucketsStr); err == nil && n > 0 && n <= 500 {
		buckets = n
	}
	rollups, err := s.deps.Store.QueryRequestSeries(r.Context(), tenant, dur, buckets)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, rollups)
}

func (s *Server) handleChartAccounts(w http.ResponseWriter, r *http.Request) {
	slots := s.deps.Sched.Snapshot()
	type bucket struct {
		StatusCategory string `json:"status_category"`
		StatusLabel    string `json:"status_label"`
		Count          int    `json:"count"`
	}
	count := map[string]int{}
	for _, sl := range slots {
		count[sl.StatusCategory]++
	}
	out := []bucket{}
	for _, k := range []string{
		scheduler.SlotStatusHealthy,
		scheduler.SlotStatusLowQuota,
		scheduler.SlotStatusNoQuota,
		scheduler.SlotStatusBanned,
		scheduler.SlotStatusAbnormal,
	} {
		out = append(out, bucket{StatusCategory: k, StatusLabel: scheduler.SlotStatusLabel(k), Count: count[k]})
	}
	writeJSONStatus(w, 200, out)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"version":   "0.1.0-mvp",
		"identity":  s.deps.Cfg.Cluster.Identity,
		"region":    s.deps.Cfg.Cluster.Region,
		"tenants":   len(s.crud.Resolver.Tenants()),
		"accounts":  len(s.deps.Sched.Snapshot()),
		"reload_at": s.crud.Resolver.LastReload(),
	}
	writeJSONStatus(w, 200, info)
}

// ----- Cluster peer endpoints (token auth) -----

func (s *Server) clusterTokenOK(r *http.Request) bool {
	tok := r.Header.Get("X-Pool-Token")
	if tok == "" {
		return false
	}
	for _, p := range s.deps.Cfg.Cluster.Peers {
		if p.Token == tok {
			return true
		}
	}
	return false
}

func (s *Server) handlePeerSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.clusterTokenOK(r) {
		errJSON(w, 401, "invalid peer token")
		return
	}
	slots := s.deps.Sched.Snapshot()
	healthy := 0
	for _, sl := range slots {
		if sl.Healthy {
			healthy++
		}
	}
	writeJSONStatus(w, 200, map[string]any{
		"identity":         s.deps.Cfg.Cluster.Identity,
		"region":           s.deps.Cfg.Cluster.Region,
		"accounts_total":   len(slots),
		"accounts_healthy": healthy,
		"time":             time.Now(),
	})
}

func (s *Server) handlePeerAccountsSummary(w http.ResponseWriter, r *http.Request) {
	if !s.clusterTokenOK(r) {
		errJSON(w, 401, "invalid peer token")
		return
	}
	writeJSONStatus(w, 200, s.deps.Sched.Snapshot())
}

func (s *Server) handleWindsurfEnroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FirebaseToken string `json:"firebase_token"`
		APIKey        string `json:"api_key"`
		TenantID      string `json:"tenant_id"`
		Note          string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}

	apiKey := req.APIKey
	if apiKey == "" && req.FirebaseToken != "" {
		var err error
		apiKey, err = windsurfPkg.RegisterUser(r.Context(), req.FirebaseToken)
		if err != nil {
			errJSON(w, 502, "register failed: "+err.Error())
			return
		}
	}
	if apiKey == "" {
		errJSON(w, 400, "provide firebase_token or api_key")
		return
	}

	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	accID := "acc-ws-" + apiKey[:8]

	a := &domain.Account{
		ID:             accID,
		TenantID:       tenantID,
		Provider:       "windsurf",
		PlanTier:       "pro",
		StealthProfile: "chrome_124_windows",
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		SessionToken: apiKey,
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Register(a)
	s.crud.Audit.Log("info", "account", accID, "", "windsurf account enrolled")
	writeJSONStatus(w, 201, map[string]string{"id": accID, "api_key": apiKey})
}

func (s *Server) handleKiroEnroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		ProfileArn   string `json:"profile_arn"`
		ProfileARN   string `json:"profileArn"`
		TenantID     string `json:"tenant_id"`
		Email        string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if req.RefreshToken == "" {
		errJSON(w, 400, "provide refresh_token")
		return
	}
	if (req.ClientID == "") != (req.ClientSecret == "") {
		errJSON(w, 400, "provide both client_id and client_secret, or neither")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	tokenHash := sha256.Sum256([]byte(req.RefreshToken))
	accID := "acc-kiro-" + fmt.Sprintf("%x", tokenHash[:])[:12]

	var oidcCreds []byte
	if req.ClientID != "" && req.ClientSecret != "" {
		profileArn := req.ProfileArn
		if profileArn == "" {
			profileArn = req.ProfileARN
		}
		oidcCreds, _ = json.Marshal(map[string]string{
			"client_id":     req.ClientID,
			"client_secret": req.ClientSecret,
			"profile_arn":   profileArn,
		})
	}

	a := &domain.Account{
		ID:        accID,
		TenantID:  tenantID,
		Provider:  "kiro",
		Email:     accountEmailFromImport(req.Email, ""),
		PlanTier:  "free",
		State:     domain.StateActive,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	sec := store.AccountSecret{
		RefreshToken: req.RefreshToken,
		Cookies:      oidcCreds,
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Register(a)
	s.crud.Audit.Log("info", "account", accID, "", "kiro account enrolled: "+req.Email)
	writeJSONStatus(w, 201, map[string]string{"id": accID})
}

var _ = errors.New

func init() {
	chiParam = func(r *http.Request, name string) string {
		return chi.URLParam(r, name)
	}
}
