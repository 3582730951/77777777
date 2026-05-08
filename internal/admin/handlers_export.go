package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

type accountExportItem struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	Provider       string    `json:"provider"`
	Email          string    `json:"email,omitempty"`
	State          string    `json:"state,omitempty"`
	PlanTier       string    `json:"plan_tier,omitempty"`
	StealthProfile string    `json:"stealth_profile,omitempty"`
	UA             string    `json:"ua,omitempty"`
	Proxy          string    `json:"proxy,omitempty"`
	SessionToken   string    `json:"session_token,omitempty"`
	RefreshToken   string    `json:"refresh_token,omitempty"`
	Cookies        string    `json:"cookies,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

func (s *Server) handleExportAccountsAPI(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant")
	if tenantID == "" {
		tenantID = r.URL.Query().Get("tenant_id")
	}
	provider := r.URL.Query().Get("provider")

	accs, err := s.deps.Store.ListAccounts(r.Context(), tenantID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]accountExportItem, 0, len(accs))
	for _, a := range accs {
		if provider != "" && a.Provider != provider {
			continue
		}
		sec, err := s.deps.Store.GetAccountSecret(r.Context(), a.ID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "export "+a.ID+": "+err.Error())
			return
		}
		items = append(items, accountExportItem{
			ID:             a.ID,
			TenantID:       a.TenantID,
			Provider:       a.Provider,
			Email:          a.Email,
			State:          string(a.State),
			PlanTier:       a.PlanTier,
			StealthProfile: a.StealthProfile,
			UA:             a.UA,
			Proxy:          a.Proxy,
			SessionToken:   sec.SessionToken,
			RefreshToken:   sec.RefreshToken,
			Cookies:        string(sec.Cookies),
			CreatedAt:      a.CreatedAt,
			UpdatedAt:      a.UpdatedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].TenantID != items[j].TenantID {
			return items[i].TenantID < items[j].TenantID
		}
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].ID < items[j].ID
	})

	filename := "accounts-export-" + time.Now().UTC().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(items)

	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "account", "", "", "accounts exported")
	}
}
