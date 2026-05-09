package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
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
