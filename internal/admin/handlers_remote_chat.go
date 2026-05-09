package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

type remoteChatConfigReq struct {
	AccountID string `json:"account_id"`
}

type remoteChatConfigResp struct {
	AccountID      string       `json:"account_id"`
	Account        *accountView `json:"account,omitempty"`
	AccountMissing bool         `json:"account_missing,omitempty"`
}

type remoteChatAPIKeyOption struct {
	Value    string
	TenantID string
	GroupID  string
	Label    string
	Provider string
	Source   string
}

func (s *Server) handleRemoteChatPage(w http.ResponseWriter, r *http.Request) {
	data := s.remoteChatPageData(r)
	data["Saved"] = r.URL.Query().Get("saved") == "1"
	data["Cleared"] = r.URL.Query().Get("cleared") == "1"
	s.render(w, r, "remote_chat.html", data)
}

func (s *Server) handleRemoteChatConfigPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		data := s.remoteChatPageData(r)
		data["Error"] = err.Error()
		s.render(w, r, "remote_chat.html", data)
		return
	}
	accountID := strings.TrimSpace(r.FormValue("account_id"))
	if accountID == "" {
		s.handleRemoteChatClearPost(w, r)
		return
	}
	if _, err := s.deps.Store.GetAccount(r.Context(), accountID); err != nil {
		data := s.remoteChatPageData(r)
		if errors.Is(err, sql.ErrNoRows) {
			data["Error"] = "账号不存在：" + accountID
		} else {
			data["Error"] = err.Error()
		}
		s.render(w, r, "remote_chat.html", data)
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingRemoteChatAccountID, accountID); err != nil {
		data := s.remoteChatPageData(r)
		data["Error"] = err.Error()
		s.render(w, r, "remote_chat.html", data)
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "remote_chat", accountID, "", "remote chat account configured via UI")
	}
	http.Redirect(w, r, "/remote-chat?saved=1", http.StatusSeeOther)
}

func (s *Server) handleRemoteChatClearPost(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Store.DeleteSetting(r.Context(), store.SettingRemoteChatAccountID); err != nil {
		data := s.remoteChatPageData(r)
		data["Error"] = err.Error()
		s.render(w, r, "remote_chat.html", data)
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "remote_chat", "", "", "remote chat account cleared via UI")
	}
	http.Redirect(w, r, "/remote-chat?cleared=1", http.StatusSeeOther)
}

func (s *Server) handleGetRemoteChatConfig(w http.ResponseWriter, r *http.Request) {
	accountID, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingRemoteChatAccountID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok || strings.TrimSpace(accountID) == "" {
		writeJSONStatus(w, http.StatusOK, remoteChatConfigResp{})
		return
	}
	resp := s.remoteChatConfigResponse(r, accountID)
	writeJSONStatus(w, http.StatusOK, resp)
}

func (s *Server) handleSetRemoteChatConfig(w http.ResponseWriter, r *http.Request) {
	var req remoteChatConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		s.handleClearRemoteChatConfig(w, r)
		return
	}
	if _, err := s.deps.Store.GetAccount(r.Context(), accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			errJSON(w, http.StatusNotFound, "account not found")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingRemoteChatAccountID, accountID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "remote_chat", accountID, "", "remote chat account configured")
	}
	writeJSONStatus(w, http.StatusOK, s.remoteChatConfigResponse(r, accountID))
}

func (s *Server) handleClearRemoteChatConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Store.DeleteSetting(r.Context(), store.SettingRemoteChatAccountID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "remote_chat", "", "", "remote chat account cleared")
	}
	writeJSONStatus(w, http.StatusOK, remoteChatConfigResp{})
}

func (s *Server) remoteChatConfigResponse(r *http.Request, accountID string) remoteChatConfigResp {
	resp := remoteChatConfigResp{AccountID: accountID}
	acc, err := s.deps.Store.GetAccount(r.Context(), accountID)
	if err != nil {
		resp.AccountMissing = true
		return resp
	}
	v := accountView{
		ID:             acc.ID,
		TenantID:       acc.TenantID,
		Provider:       acc.Provider,
		Email:          acc.Email,
		State:          string(acc.State),
		PlanTier:       acc.PlanTier,
		StealthProfile: acc.StealthProfile,
		UA:             acc.UA,
		Proxy:          acc.Proxy,
		Models:         acc.Quota.DiscoveredModels,
	}
	if s.deps.Sched != nil {
		for _, sl := range s.deps.Sched.Snapshot() {
			if sl.AccountID == accountID {
				applySlotToAccountView(&v, sl)
				resp.Account = &v
				return resp
			}
		}
	}
	applySlotToAccountView(&v, scheduler.SlotViewFromAccount(acc))
	resp.Account = &v
	return resp
}

func (s *Server) remoteChatPageData(r *http.Request) map[string]any {
	accountID, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingRemoteChatAccountID)
	var cfg remoteChatConfigResp
	if err == nil && ok && strings.TrimSpace(accountID) != "" {
		cfg = s.remoteChatConfigResponse(r, accountID)
	}
	accounts := s.remoteChatAccounts(r)
	keys := s.remoteChatAPIKeys(r)
	models := []string{}
	if cfg.Account != nil {
		models = remoteChatModelsForAccount(cfg.Account)
	} else if len(accounts) > 0 {
		models = remoteChatModelsForAccount(&accounts[0])
	}
	data := map[string]any{
		"Active":          "remote-chat",
		"Title":           "在线聊天",
		"Config":          cfg,
		"Accounts":        accounts,
		"APIKeys":         keys,
		"GatewayURL":      s.gatewayURL(r),
		"SuggestedModels": models,
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	return data
}

func (s *Server) remoteChatAccounts(r *http.Request) []accountView {
	accs, err := s.deps.Store.ListAccounts(r.Context(), "")
	if err != nil {
		return nil
	}
	slotByID := map[string]scheduler.SlotView{}
	if s.deps.Sched != nil {
		for _, sl := range s.deps.Sched.Snapshot() {
			slotByID[sl.AccountID] = sl
		}
	}
	views := make([]accountView, 0, len(accs))
	for _, acc := range accs {
		v := accountView{
			ID:             acc.ID,
			TenantID:       acc.TenantID,
			Provider:       acc.Provider,
			Email:          acc.Email,
			State:          string(acc.State),
			PlanTier:       acc.PlanTier,
			StealthProfile: acc.StealthProfile,
			UA:             acc.UA,
			Proxy:          acc.Proxy,
			Models:         acc.Quota.DiscoveredModels,
		}
		if sl, ok := slotByID[acc.ID]; ok {
			applySlotToAccountView(&v, sl)
		} else {
			applySlotToAccountView(&v, scheduler.SlotViewFromAccount(acc))
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].SortRank != views[j].SortRank {
			return views[i].SortRank < views[j].SortRank
		}
		if views[i].Provider != views[j].Provider {
			return views[i].Provider < views[j].Provider
		}
		return views[i].ID < views[j].ID
	})
	return views
}

func (s *Server) remoteChatAPIKeys(r *http.Request) []remoteChatAPIKeyOption {
	groupProvider := map[string]string{}
	dynGroups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	for _, g := range s.deps.Cfg.Groups {
		groupProvider[g.ID] = g.Provider
	}
	for _, g := range dynGroups {
		groupProvider[g.ID] = g.Provider
	}
	keys, _ := s.deps.Store.ListAPIKeys(r.Context(), "", "")
	out := make([]remoteChatAPIKeyOption, 0, len(keys))
	seen := map[string]bool{}
	add := func(value, tenantID, groupID, label, source string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, remoteChatAPIKeyOption{
			Value:    value,
			TenantID: tenantID,
			GroupID:  groupID,
			Label:    label,
			Provider: groupProvider[groupID],
			Source:   source,
		})
	}
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		add(k.Value, k.TenantID, k.GroupID, k.Label, "db")
	}
	for _, g := range s.deps.Cfg.Groups {
		for _, key := range g.APIKeys {
			add(key, g.TenantID, g.ID, "config.yaml", "yaml")
		}
	}
	return out
}

func remoteChatModelsForAccount(acc *accountView) []string {
	if acc == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	for _, model := range acc.Models {
		if model.Available {
			add(model.ID)
		}
	}
	for _, model := range acc.Models {
		add(model.ID)
	}
	for _, model := range acc.DiscoveredModels {
		add(model)
	}
	add(remoteChatDefaultModelForProvider(acc.Provider))
	return out
}

func remoteChatDefaultModelForProvider(provider string) string {
	switch provider {
	case "chatgpt", "blink":
		return "gpt-5.2"
	case "claude", "kiro", "windsurf", "trae":
		return "claude-sonnet-4-6"
	case "gemini":
		return "gemini-2.5-pro"
	case "grok":
		return "grok-4"
	case "cursor", "openblocklabs":
		return "gpt-4o"
	case "cerebras":
		return "llama-3.3-70b"
	default:
		return ""
	}
}
