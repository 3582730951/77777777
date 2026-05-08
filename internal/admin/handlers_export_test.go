package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
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
