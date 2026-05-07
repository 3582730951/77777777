package detection

import (
	"context"
	"log"
	"time"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

// AutoRemover periodically checks for banned accounts and removes them from the
// pool (scheduler + persistent store). This prevents banned accounts from being
// retried or consuming discovery cycles.
type AutoRemover struct {
	sched    *scheduler.Scheduler
	store    *store.Store
	audit    *audit.Logger
	interval time.Duration
}

func NewAutoRemover(sched *scheduler.Scheduler, st *store.Store, auditL *audit.Logger) *AutoRemover {
	return &AutoRemover{
		sched:    sched,
		store:    st,
		audit:    auditL,
		interval: 30 * time.Second,
	}
}

// RemoveNow removes one banned account immediately. It is intended for the
// scheduler banned hook so hard-banned accounts leave the pool without waiting
// for the periodic sweep.
func (a *AutoRemover) RemoveNow(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.remove(ctx, id)
}

func (a *AutoRemover) Run(ctx context.Context) {
	// Check immediately on start.
	a.sweep(ctx)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweep(ctx)
		}
	}
}

func (a *AutoRemover) sweep(ctx context.Context) {
	ids := a.sched.BannedAccountIDs()
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		a.remove(ctx, id)
	}
}

func (a *AutoRemover) remove(ctx context.Context, id string) {
	acc, ok := a.sched.AccountByID(id)
	if !ok {
		return
	}
	if acc == nil || acc.State != "banned" {
		return
	}
	provider := acc.Provider

	if err := a.store.DeleteAccount(ctx, id); err != nil {
		log.Printf("[auto-remove] failed to delete account %s from store: %v", id, err)
		return
	}
	a.sched.Unregister(id)

	msg := "auto-removed banned account: " + id + " (provider=" + provider + ")"
	log.Printf("[auto-remove] %s", msg)
	if a.audit != nil {
		a.audit.Log("warn", "auto-remove", id, "", msg)
	}
}
