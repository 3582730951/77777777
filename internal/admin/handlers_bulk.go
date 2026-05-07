package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

// Bulk import endpoints.

// AccountsBulkImport: paste an array of {provider, plan_tier, session_token, cookies, ua, proxy}
// objects, or a Netscape cookies.txt blob with provider/tenant_id headers.
func (s *Server) handleAccountsBulkImport(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant")
	if tenantID == "" {
		tenantID = "default"
	}
	body, _ := io.ReadAll(r.Body)
	created := []string{}

	// Try JSON-array first.
	var arr []createAccountReq
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		for _, req := range arr {
			id := req.ID
			if id == "" {
				id = "acc-" + randHex(6)
			}
			if req.TenantID == "" {
				req.TenantID = tenantID
			}
			if req.StealthProfile == "" {
				req.StealthProfile = "chrome_124_windows"
			}
			a := &domain.Account{
				ID:             id,
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
				continue
			}
			s.deps.Sched.Register(a)
			created = append(created, id)
		}
		writeJSONStatus(w, 200, map[string]any{"created": created, "count": len(created), "format": "json"})
		return
	}

	// Otherwise treat as Netscape cookies.txt: produce ONE account holding
	// all those cookies; provider must come from the query string.
	provider := strings.ToLower(r.URL.Query().Get("provider"))
	if provider == "" {
		errJSON(w, 400, "for cookies.txt format, ?provider= is required")
		return
	}
	id := "acc-" + randHex(6)
	a := &domain.Account{
		ID:             id,
		TenantID:       tenantID,
		Provider:       provider,
		StealthProfile: "chrome_124_windows",
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{Cookies: body}
	if err := s.deps.Store.UpsertAccount(r.Context(), a, sec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.deps.Sched.Register(a)
	created = append(created, id)
	writeJSONStatus(w, 200, map[string]any{"created": created, "count": 1, "format": "cookies.txt"})
}

// AcceptClusterConfig: receives a config dump pushed by another peer.
// Tenants/groups/keys/accounts are upserted (account credentials are NOT
// transferred — the receiving instance must enroll its own).
func (s *Server) handleAcceptClusterConfig(w http.ResponseWriter, r *http.Request) {
	if !s.clusterTokenOK(r) {
		errJSON(w, 401, "invalid peer token")
		return
	}
	var payload struct {
		Tenants  []store.Tenant       `json:"tenants"`
		Groups   []store.DynGroup     `json:"groups"`
		Accounts []*domain.Account    `json:"accounts"`
		Keys     []store.APIKeyRecord `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	count := 0
	for _, t := range payload.Tenants {
		_ = s.deps.Store.UpsertDynTenant(r.Context(), t)
		count++
	}
	for _, g := range payload.Groups {
		_ = s.deps.Store.UpsertDynGroup(r.Context(), g)
		count++
	}
	for _, a := range payload.Accounts {
		// Don't transfer credentials between peers; we register the metadata
		// only so the operator can re-enroll secrets locally if desired.
		_ = s.deps.Store.UpsertAccount(r.Context(), a, store.AccountSecret{})
		count++
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "cluster", "", "", "config accepted: "+itoa(count)+" objects")
	}
	if s.crud.Resolver != nil {
		_ = s.crud.Resolver.Reload(r.Context(), s.deps.Cfg)
	}
	writeJSONStatus(w, 200, map[string]any{"accepted": count})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
