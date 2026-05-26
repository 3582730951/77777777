package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

func TestHandleTestAllAccountsSkipsNoQuotaAndMarksFailuresAbnormal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sched := scheduler.New(config.Scheduler{})
	for _, acc := range []*domain.Account{
		testAccountWithQuota("acc-ok", 100, 10),
		testAccountWithQuota("acc-fail", 100, 10),
		testAccountWithQuota("acc-noquota", 100, 100),
	} {
		if err := st.UpsertAccount(t.Context(), acc, store.AccountSecret{}); err != nil {
			t.Fatalf("upsert account: %v", err)
		}
		sched.Register(acc)
	}

	var noQuotaCalls atomic.Int32
	s := &Server{
		deps: Deps{
			Store: st,
			Sched: sched,
			AccountTestFunc: func(ctx context.Context, accountID string) error {
				switch accountID {
				case "acc-ok":
					return nil
				case "acc-fail":
					return errors.New("network smoke failure")
				case "acc-noquota":
					noQuotaCalls.Add(1)
					return nil
				default:
					t.Fatalf("unexpected account test call for %s", accountID)
					return nil
				}
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/test-all", nil)
	rec := httptest.NewRecorder()
	s.handleTestAllAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool `json:"ok"`
		Tested    int  `json:"tested"`
		Succeeded int  `json:"succeeded"`
		Failed    int  `json:"failed"`
		Skipped   int  `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK || resp.Tested != 2 || resp.Succeeded != 1 || resp.Failed != 1 || resp.Skipped != 1 {
		t.Fatalf("unexpected response: %+v body=%s", resp, rec.Body.String())
	}
	if got := noQuotaCalls.Load(); got != 0 {
		t.Fatalf("no quota account was tested %d times", got)
	}

	byID := map[string]scheduler.SlotView{}
	for _, sl := range sched.Snapshot() {
		byID[sl.AccountID] = sl
	}
	if got := byID["acc-fail"].StatusCategory; got != scheduler.SlotStatusAbnormal {
		t.Fatalf("failed account status = %q, want abnormal", got)
	}
	if got := byID["acc-noquota"].StatusCategory; got != scheduler.SlotStatusNoQuota {
		t.Fatalf("no quota account status = %q, want no_quota", got)
	}
	if got := byID["acc-ok"].StatusCategory; got != scheduler.SlotStatusHealthy {
		t.Fatalf("ok account status = %q, want healthy", got)
	}
}

func TestAccountPoolTestFailureClassPreservesQuotaExhaustion(t *testing.T) {
	if got := accountPoolTestFailureClass(errors.New("you've hit your usage limit, try again later")); got != domain.ErrQuotaExhausted {
		t.Fatalf("class = %q, want quota exhausted", got)
	}
	if got := accountPoolTestFailureClass(errors.New("temporary upstream EOF")); got != domain.ErrAuthFailed {
		t.Fatalf("class = %q, want auth failed to force abnormal bucket", got)
	}
}

func TestNormalizeChatGPTAccountSecretClearsSessionOnlyRecoveryMaterial(t *testing.T) {
	sessionOnly := `{"auth_mode":"chatgpt","session_import_mode":"session_only_json","access_token":"access-1","refresh_token":"","tokens":{"access_token":"access-1","refresh_token":"","id_token_synthetic":true}}`
	sec := normalizeChatGPTAccountSecret("chatgpt", store.AccountSecret{
		SessionToken: sessionOnly,
		RefreshToken: "rt-must-drop",
		Cookies:      []byte("__Secure-next-auth.session-token=must-drop"),
	})

	if sec.RefreshToken != "" || len(sec.Cookies) != 0 {
		t.Fatalf("session-only secret retained recovery material: %+v", sec)
	}
	if sec.SessionToken != sessionOnly {
		t.Fatalf("session token changed: %q", sec.SessionToken)
	}
}

func TestNormalizeChatGPTAccountSecretKeepsWebSessionCookies(t *testing.T) {
	sec := normalizeChatGPTAccountSecret("chatgpt", store.AccountSecret{
		SessionToken: `{"accessToken":"access-1","expires":"2099-01-01T00:00:00Z"}`,
		Cookies:      []byte("__Secure-next-auth.session-token=keep"),
	})

	if string(sec.Cookies) != "__Secure-next-auth.session-token=keep" {
		t.Fatalf("web-session cookies should be preserved: %+v", sec)
	}
}

func TestDecodeAccountImportRequestsAcceptsSingleCPAAuthJSON(t *testing.T) {
	body := []byte(`{
		"type":"codex",
		"email":"cpa@example.com",
		"account_id":"acct-cpa",
		"chatgpt_plan_type":"plus",
		"access_token":"access-1",
		"refresh_token":"",
		"session_token":"next-auth-cpa",
		"id_token_synthetic":true
	}`)

	reqs, ok, err := decodeAccountImportRequests(body, "default")
	if err != nil {
		t.Fatalf("decode import: %v", err)
	}
	if !ok || len(reqs) != 1 {
		t.Fatalf("decode import ok=%v len=%d", ok, len(reqs))
	}
	req := reqs[0]
	if req.Provider != "chatgpt" || req.ID != "acct-cpa" || req.Email != "cpa@example.com" || req.PlanTier != "plus" {
		t.Fatalf("normalized metadata mismatch: %+v", req)
	}
	if !strings.Contains(req.SessionToken, `"access_token":"access-1"`) || !strings.Contains(req.SessionToken, `"session_token":"next-auth-cpa"`) {
		t.Fatalf("CPA auth JSON should be stored intact as session token: %s", req.SessionToken)
	}
}

func testAccountWithQuota(id string, limit, used float64) *domain.Account {
	return &domain.Account{
		ID:       id,
		TenantID: "default",
		Provider: "chatgpt",
		State:    domain.StateActive,
		Quota: domain.QuotaState{
			ShortWindow: domain.QuotaWindow{Limit: limit, Used: used},
			LongWindow:  domain.QuotaWindow{Limit: limit, Used: used},
		},
	}
}
