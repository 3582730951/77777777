package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

// ---- Account form handlers (HTML <form> POSTs from the UI) ----

func (s *Server) handleAccountForm(w http.ResponseWriter, r *http.Request) {
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	yamlGroups := s.deps.Cfg.Groups
	s.render(w, r, "account_form.html", map[string]any{
		"Active":      "accounts",
		"Title":       "添加账号",
		"Account":     &domain.Account{Provider: "chatgpt", PlanTier: "plus"},
		"Groups":      groups,
		"YamlGroups":  yamlGroups,
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
	})
}

func (s *Server) handleAccountCreatePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		id = "acc-" + randHex(6)
	}
	tenantID := r.FormValue("tenant_id")
	if tenantID == "" {
		tenantID = "default"
	}
	a := &domain.Account{
		ID:             id,
		TenantID:       tenantID,
		Provider:       r.FormValue("provider"),
		Email:          accountEmailFromImport(r.FormValue("email"), r.FormValue("session_token")),
		PlanTier:       r.FormValue("plan_tier"),
		StealthProfile: defaultStr(r.FormValue("stealth_profile"), "chrome_124_windows"),
		UA:             r.FormValue("ua"),
		Proxy:          r.FormValue("proxy"),
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		Cookies:      []byte(r.FormValue("cookies")),
		SessionToken: r.FormValue("session_token"),
		RefreshToken: r.FormValue("refresh_token"),
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.deps.Sched.Register(a)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "account", id, "", "account added via UI")
	}
	http.Redirect(w, r, "/accounts/"+url.PathEscape(id), http.StatusSeeOther)
}

func (s *Server) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.deps.Sched.AccountByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	slots := s.deps.Sched.Snapshot()
	var slot any
	for _, sl := range slots {
		if sl.AccountID == id {
			slot = sl
		}
	}
	samples, _ := s.deps.Store.QueryQuotaSamples(r.Context(), id, time.Now().Add(-6*time.Hour), 720)
	s.render(w, r, "account_detail.html", map[string]any{
		"Active":  "accounts",
		"Title":   "账号详情 · " + id,
		"Account": a,
		"Slot":    slot,
		"Samples": samples,
	})
}

func (s *Server) handleAccountProbeForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	log.Printf("[probe] account=%s started", id)

	// Step 1: Probe (verify connectivity)
	if s.deps.ProbeFunc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.deps.ProbeFunc(ctx, id); err != nil {
			log.Printf("[probe] account=%s probe failed: %v", id, err)
			if s.crud.Audit != nil {
				s.crud.Audit.Log("warn", "probe", id, "", "manual probe fail: "+err.Error())
			}
		} else {
			log.Printf("[probe] account=%s probe ok", id)
			s.deps.Sched.MarkSuccess(id, 100)
		}
	}

	// Step 2: Discover (always run, even if probe fails — quota refresh is independent)
	if s.deps.DiscoverFunc != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dcancel()
		state, derr := s.deps.DiscoverFunc(dctx, id)
		if derr != nil {
			log.Printf("[probe] account=%s discover failed: %v", id, derr)
		} else if state != nil {
			s.deps.Sched.UpdateQuota(id, state)
			log.Printf("[probe] account=%s discover ok: 5h=%.0f/%.0f 7d=%.0f/%.0f",
				id, state.ShortWindow.Used, state.ShortWindow.Limit,
				state.LongWindow.Used, state.LongWindow.Limit)
		}
		if s.crud.Audit != nil {
			s.crud.Audit.Log("info", "probe", id, "", "manual probe+discover done")
		}
	}

	http.Redirect(w, r, "/accounts/"+url.PathEscape(id), http.StatusSeeOther)
}

func (s *Server) handleAccountDeleteForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.deps.Store.DeleteAccount(r.Context(), id)
	s.deps.Sched.Unregister(id)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "account", id, "", "account deleted via UI")
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// ---- Group form handlers ----

// allModelsForProvider collects all known models for a provider from:
// 1. Scheduler discovered models (live account pool)
// 2. Config YAML group model lists
// 3. Hard-coded per-provider fallback list
func (s *Server) allModelsForProvider(provider string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	// 1. Live discovered from scheduler
	for _, mc := range s.deps.Sched.AvailableModels(provider) {
		add(mc.ID)
	}
	// 2. Config YAML groups for this provider
	for _, g := range s.deps.Cfg.Groups {
		if g.Provider == provider {
			for _, m := range g.Models {
				add(m)
			}
			for _, m := range g.ModelWhitelist {
				add(m)
			}
		}
	}
	// 3. Hard-coded fallback per provider (from CPA models.json)
	fallbacks := map[string][]string{
		"chatgpt": {"gpt-5.2", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-image-2", "codex-auto-review"},
		"claude":  {"claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5-20251101", "claude-opus-4-20250514", "claude-sonnet-4-6", "claude-sonnet-4-5-20250929", "claude-sonnet-4-20250514", "claude-3-7-sonnet-20250219", "claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022"},
		"gemini":  {"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-3-pro-preview", "gemini-3.1-pro-preview", "gemini-3-flash-preview", "gemini-3.1-flash-lite-preview"},
	}
	for _, m := range fallbacks[provider] {
		add(m)
	}
	return out
}

func (s *Server) handleGroupForm(w http.ResponseWriter, r *http.Request) {
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	s.render(w, r, "group_form.html", map[string]any{
		"Active":      "groups",
		"Title":       "添加分组",
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
		"AllModelsByProv": map[string][]string{
			"chatgpt": s.allModelsForProvider("chatgpt"),
			"claude":  s.allModelsForProvider("claude"),
			"gemini":  s.allModelsForProvider("gemini"),
		},
	})
}

func (s *Server) handleGroupCreatePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "id required", 400)
		return
	}
	models := splitCSV(r.FormValue("models"))
	whitelist := splitCSV(r.FormValue("model_whitelist"))
	aliases := parseAliasMap(r.FormValue("model_aliases"))
	g := store.DynGroup{
		ID:               id,
		TenantID:         defaultStr(r.FormValue("tenant_id"), "default"),
		Provider:         r.FormValue("provider"),
		Models:           models,
		ModelAliases:     aliases,
		ModelWhitelist:   whitelist,
		AccountIDs:       splitCSV(r.FormValue("account_ids")),
		SystemPrompt:     r.FormValue("system_prompt"),
		SystemPromptMode: defaultStr(r.FormValue("system_prompt_mode"), "prepend"),
		ReasoningEffort:  r.FormValue("reasoning_effort"),
		ForcedModel:      r.FormValue("forced_model"),
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.deps.Store.UpsertDynGroup(r.Context(), g); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "group", "", id, "group added via UI")
	}
	http.Redirect(w, r, "/groups", http.StatusSeeOther)
}

func (s *Server) handleGroupDeleteForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.deps.Store.DeleteDynGroup(r.Context(), id)
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "group", "", id, "group deleted via UI")
	}
	http.Redirect(w, r, "/groups", http.StatusSeeOther)
}

// ---- Tenant pages ----

func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	dyns, _ := s.deps.Store.ListDynTenants(r.Context())
	users, _ := s.deps.Store.ListTenantUsers(r.Context(), "")
	usersByTenant := map[string][]store.TenantUser{}
	for _, u := range users {
		usersByTenant[u.TenantID] = append(usersByTenant[u.TenantID], u)
	}
	flashUser := r.URL.Query().Get("flash_user")
	flashPW := ""
	if flashUser != "" {
		flashPW = tenantUserFlashStore.take(flashUser)
	}
	s.render(w, r, "tenants.html", map[string]any{
		"Active":        "tenants",
		"Title":         "租户",
		"Dyns":          dyns,
		"YamlTenants":   s.deps.Cfg.Tenants,
		"UsersByTenant": usersByTenant,
		"FlashUser":     flashUser,
		"FlashPW":       flashPW,
	})
}

func (s *Server) handleTenantCreatePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "id required", 400)
		return
	}
	name := defaultStr(r.FormValue("name"), id)
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		username = id
	}
	password := strings.TrimSpace(r.FormValue("password"))
	if password == "" {
		password = genTenantPassword()
	}
	_ = s.deps.Store.UpsertDynTenant(r.Context(), store.Tenant{ID: id, Name: name, CreatedAt: time.Now()})
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := s.deps.Store.CreateTenantUser(r.Context(), username, id, string(hash)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "tenant", "", "", "tenant+user created via UI: "+id+"/"+username)
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	tenantUserFlashStore.set(username, password)
	http.Redirect(w, r, "/tenants?flash_user="+url.QueryEscape(username), http.StatusSeeOther)
}

func (s *Server) handleTenantDeleteForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.deps.Store.DeleteDynTenant(r.Context(), id)
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "tenant", "", "", "tenant deleted via UI: "+id)
	}
	http.Redirect(w, r, "/tenants", http.StatusSeeOther)
}

func (s *Server) handleTenantRotateForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	mk, _ := s.deps.Store.SetTenantMasterKey(r.Context(), id)
	tenantFlashStore.set(id, mk)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "tenant", "", "", "master key rotated: "+id)
	}
	http.Redirect(w, r, "/tenants?flash="+url.QueryEscape(id), http.StatusSeeOther)
}

// ---- Key pages ----

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	keys, _ := s.deps.Store.ListAPIKeys(r.Context(), "", "")
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	flashKey := r.URL.Query().Get("flash")
	flashVal := ""
	if flashKey != "" {
		flashVal = keyFlashStore.take(flashKey)
	}
	s.render(w, r, "keys.html", map[string]any{
		"Active":      "keys",
		"Title":       "API Keys",
		"Keys":        keys,
		"Groups":      groups,
		"YamlGroups":  s.deps.Cfg.Groups,
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
		"FlashKey":    flashVal,
		"GatewayURL":  s.gatewayURL(r),
	})
}

func (s *Server) handleKeyCreatePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tenantID := r.FormValue("tenant_id")
	groupID := r.FormValue("group_id")
	label := r.FormValue("label")
	if tenantID == "" || groupID == "" {
		http.Error(w, "tenant + group required", 400)
		return
	}
	rec, err := s.deps.Store.CreateAPIKey(r.Context(), tenantID, groupID, label)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "apikey", "", groupID, "key created via UI: "+rec.Label)
	}
	keyFlashStore.set(rec.Value, rec.Value)
	http.Redirect(w, r, "/keys?flash="+url.QueryEscape(rec.Value), http.StatusSeeOther)
}

func (s *Server) handleKeyRevokeForm(w http.ResponseWriter, r *http.Request) {
	val := chi.URLParam(r, "value")
	_ = s.deps.Store.RevokeAPIKey(r.Context(), val)
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "apikey", "", "", "key revoked via UI")
	}
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

// ---- Audit page ----

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	entries, _ := s.deps.Store.QueryAudit(r.Context(), 200, 0)
	s.render(w, r, "audit.html", map[string]any{
		"Active":  "audit",
		"Title":   "审计日志",
		"Entries": entries,
	})
}

// ---- Helpers ----

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseAliasMap accepts either "k1=v1,k2=v2" or JSON object.
func parseAliasMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "{") {
		out := map[string]string{}
		_ = json.Unmarshal([]byte(s), &out)
		return out
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = cryptoRandRead(b)
	return hex.EncodeToString(b)
}

// indirection helps tests; real impl uses crypto/rand.
var cryptoRandRead = func(b []byte) (int, error) {
	return cryptoRandReadImpl(b)
}

// flash stores temporary one-shot UI messages keyed by some ID.
type flashStore struct {
	m map[string]flashEntry
}
type flashEntry struct {
	value     string
	createdAt time.Time
}

func (f *flashStore) set(key, value string) {
	f.m[key] = flashEntry{value: value, createdAt: time.Now()}
	// gc
	for k, e := range f.m {
		if time.Since(e.createdAt) > 5*time.Minute {
			delete(f.m, k)
		}
	}
}
func (f *flashStore) take(key string) string {
	e, ok := f.m[key]
	if !ok {
		return ""
	}
	delete(f.m, key)
	return e.value
}

var (
	tenantFlashStore = &flashStore{m: map[string]flashEntry{}}
	keyFlashStore    = &flashStore{m: map[string]flashEntry{}}
)

func cryptoRandReadImpl(b []byte) (int, error) {
	// real implementation lives in crypto/rand; declared in admin.go via cryptorand.
	// We can't import cryptorand here too without alias collision; use a wrapper.
	return cryptoRandWrapper(b)
}

// cryptoRandWrapper is set from admin.go init.
var cryptoRandWrapper = func(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> uint(i&7))
	}
	return len(b), nil
}

// initial unused warning suppressors
var _ = fmt.Sprintf

// ---- Key form handler (GET) ----

func (s *Server) handleKeyForm(w http.ResponseWriter, r *http.Request) {
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	flashKey := r.URL.Query().Get("flash")
	flashVal := ""
	if flashKey != "" {
		flashVal = keyFlashStore.take(flashKey)
	}
	s.render(w, r, "key_form.html", map[string]any{
		"Active":      "keys",
		"Title":       "生成 API Key",
		"Groups":      groups,
		"YamlGroups":  s.deps.Cfg.Groups,
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
		"FlashKey":    flashVal,
	})
}

// ---- Tenant form handler (GET) ----

func (s *Server) handleTenantForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "tenant_form.html", map[string]any{
		"Active": "tenants",
		"Title":  "新建租户",
	})
}

// ---- Group edit handlers ----

func (s *Server) handleGroupEditForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	// Try dynamic group first
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	var found *store.DynGroup
	for i := range groups {
		if groups[i].ID == id {
			found = &groups[i]
			break
		}
	}
	if found == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "group_form.html", map[string]any{
		"Active":      "groups",
		"Title":       "编辑分组 · " + id,
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
		"Group":       found,
		"FormAction":  "/groups/" + url.PathEscape(id) + "/edit",
		"IsEdit":      true,
		"AllModelsByProv": map[string][]string{
			"chatgpt": s.allModelsForProvider("chatgpt"),
			"claude":  s.allModelsForProvider("claude"),
			"gemini":  s.allModelsForProvider("gemini"),
		},
	})
}

func (s *Server) handleGroupEditPost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	models := splitCSV(r.FormValue("models"))
	whitelist := splitCSV(r.FormValue("model_whitelist"))
	aliases := parseAliasMap(r.FormValue("model_aliases"))
	g := store.DynGroup{
		ID:               id,
		TenantID:         defaultStr(r.FormValue("tenant_id"), "default"),
		Provider:         r.FormValue("provider"),
		Models:           models,
		ModelAliases:     aliases,
		ModelWhitelist:   whitelist,
		AccountIDs:       splitCSV(r.FormValue("account_ids")),
		SystemPrompt:     r.FormValue("system_prompt"),
		SystemPromptMode: defaultStr(r.FormValue("system_prompt_mode"), "prepend"),
		ReasoningEffort:  r.FormValue("reasoning_effort"),
		ForcedModel:      r.FormValue("forced_model"),
		UpdatedAt:        time.Now(),
	}
	if err := s.deps.Store.UpsertDynGroup(r.Context(), g); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "group", "", id, "group updated via UI")
	}
	http.Redirect(w, r, "/groups", http.StatusSeeOther)
}

// ---- Account edit handlers ----

func (s *Server) handleAccountEditForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.deps.Sched.AccountByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	s.render(w, r, "account_form.html", map[string]any{
		"Active":      "accounts",
		"Title":       "编辑账号 · " + id,
		"Account":     a,
		"Tenants":     tenants,
		"YamlTenants": s.deps.Cfg.Tenants,
		"FormAction":  "/accounts/" + url.PathEscape(id) + "/edit",
		"IsEdit":      true,
	})
}

func (s *Server) handleAccountEditPost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a, ok := s.deps.Sched.AccountByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.TenantID = defaultStr(r.FormValue("tenant_id"), a.TenantID)
	a.Provider = defaultStr(r.FormValue("provider"), a.Provider)
	a.Email = accountEmailFromImport(r.FormValue("email"), r.FormValue("session_token"))
	a.PlanTier = r.FormValue("plan_tier")
	a.StealthProfile = defaultStr(r.FormValue("stealth_profile"), a.StealthProfile)
	a.UA = r.FormValue("ua")
	a.Proxy = r.FormValue("proxy")
	a.UpdatedAt = time.Now()
	sec := store.AccountSecret{}
	if st := r.FormValue("session_token"); st != "" {
		sec.SessionToken = st
	}
	if rt := r.FormValue("refresh_token"); rt != "" {
		sec.RefreshToken = rt
	}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.deps.Sched.Register(a)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "account", id, "", "account updated via UI")
	}
	http.Redirect(w, r, "/accounts/"+url.PathEscape(id), http.StatusSeeOther)
}
