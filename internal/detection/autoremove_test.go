package detection

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

func TestAutoRemoverRemoveNowDeletesBannedAccount(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:        "a1",
		TenantID:  "t1",
		Provider:  "chatgpt",
		State:     domain.StateActive,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{SessionToken: "tok"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	sched := scheduler.New(config.Scheduler{})
	sched.Register(acc)
	remover := NewAutoRemover(sched, st, nil)

	sched.MarkFailure("a1", domain.ErrBanned)
	remover.RemoveNow("a1")

	if _, ok := sched.AccountByID("a1"); ok {
		t.Fatal("banned account should be removed from scheduler")
	}
	accounts, err := st.ListAccounts(ctx, "")
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("banned account should be deleted from store, got %d accounts", len(accounts))
	}
}
