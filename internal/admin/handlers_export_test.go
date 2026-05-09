package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

func TestHandleExportAccountsAPIIncludesSecrets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:             "acc-1",
		TenantID:       "default",
		Provider:       "kiro",
		Email:          "u@example.com",
		PlanTier:       "kiro",
		StealthProfile: "chrome_124_windows",
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		SessionToken: "session-json",
		RefreshToken: "refresh-token",
		Cookies:      []byte(`{"client_id":"cid","client_secret":"secret"}`),
	}
	if err := st.UpsertAccount(t.Context(), acc, sec); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	s := &Server{deps: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export?provider=kiro", nil)
	rec := httptest.NewRecorder()
	s.handleExportAccountsAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "accounts-export-") {
		t.Fatalf("missing export filename: %q", cd)
	}
	var got []accountExportItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("exported %d accounts, want 1", len(got))
	}
	if got[0].ID != "acc-1" || got[0].Provider != "kiro" {
		t.Fatalf("wrong account exported: %+v", got[0])
	}
	if got[0].SessionToken != sec.SessionToken || got[0].RefreshToken != sec.RefreshToken || got[0].Cookies != string(sec.Cookies) {
		t.Fatalf("secrets not exported: %+v", got[0])
	}
}

func TestHandleKiroEnrollPersistsOIDCProfileArn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	s := &Server{
		deps: Deps{
			Store: st,
			Sched: scheduler.New(config.Scheduler{}),
		},
		crud: CrudDeps{Audit: audit.NewLogger(st)},
	}
	reqBody := strings.NewReader(`{
		"refresh_token":"rt",
		"client_id":"cid",
		"client_secret":"secret",
		"profile_arn":"profile-arn-1",
		"email":"kiro@example.com"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/kiro/enroll", reqBody)
	rec := httptest.NewRecorder()
	s.handleKiroEnroll(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	sec, err := st.GetAccountSecret(t.Context(), resp.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	var meta map[string]string
	if err := json.Unmarshal(sec.Cookies, &meta); err != nil {
		t.Fatalf("decode cookies metadata: %v", err)
	}
	if meta["client_id"] != "cid" || meta["client_secret"] != "secret" || meta["profile_arn"] != "profile-arn-1" {
		t.Fatalf("unexpected kiro metadata: %#v", meta)
	}
}

func TestHandleRemoteChatConfigPersistsAccountID(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:       "acc-chat",
		TenantID: "default",
		Provider: "kiro",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(t.Context(), acc, store.AccountSecret{}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	s := &Server{
		deps: Deps{
			Store: st,
			Sched: scheduler.New(config.Scheduler{}),
		},
		crud: CrudDeps{Audit: audit.NewLogger(st)},
	}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/remote-chat/config", strings.NewReader(`{"account_id":"acc-chat"}`))
	rec := httptest.NewRecorder()
	s.handleSetRemoteChatConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	value, ok, err := st.GetSetting(t.Context(), store.SettingRemoteChatAccountID)
	if err != nil {
		t.Fatalf("get setting: %v", err)
	}
	if !ok || value != "acc-chat" {
		t.Fatalf("setting = %q ok=%v", value, ok)
	}
	var resp remoteChatConfigResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccountID != "acc-chat" || resp.Account == nil || resp.Account.ID != "acc-chat" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHandleTokenOptimizerSettingsPostPersistsAndUpdatesRuntimeConfig(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Root{}
	cfg.TokenOptimizer = config.NormalizeTokenOptimizer(config.TokenOptimizer{})
	s := &Server{
		deps: Deps{
			Cfg:   cfg,
			Store: st,
		},
		crud: CrudDeps{Audit: audit.NewLogger(st)},
	}

	form := "mode=guarded&min_tool_output_bytes=1024&max_optimized_tool_output_bytes=4096&head_lines=8&tail_lines=9&error_context_lines=2"
	req := httptest.NewRequest(http.MethodPost, "/settings/token-optimizer", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleTokenOptimizerSettingsPost(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if cfg.TokenOptimizer.Mode != "guarded" ||
		cfg.TokenOptimizer.MinToolOutputBytes != 1024 ||
		cfg.TokenOptimizer.MaxOptimizedToolOutputBytes != 4096 ||
		cfg.TokenOptimizer.HeadLines != 8 ||
		cfg.TokenOptimizer.TailLines != 9 ||
		cfg.TokenOptimizer.ErrorContextLines != 2 {
		t.Fatalf("runtime config not updated: %+v", cfg.TokenOptimizer)
	}
	value, ok, err := st.GetSetting(t.Context(), store.SettingTokenOptimizer)
	if err != nil {
		t.Fatalf("get setting: %v", err)
	}
	if !ok {
		t.Fatal("token optimizer setting not persisted")
	}
	var stored config.TokenOptimizer
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		t.Fatalf("decode setting: %v", err)
	}
	if stored.Mode != "guarded" || stored.MinToolOutputBytes != 1024 {
		t.Fatalf("unexpected stored setting: %+v", stored)
	}
}
