package tokenrefresh

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

const (
	defaultInterval = 5 * time.Minute
	defaultTimeout  = 30 * time.Second
	defaultWorkers  = 4
)

// CredentialRefresher is implemented by providers that can refresh persisted
// OAuth credentials without consuming normal request quota.
type CredentialRefresher interface {
	RefreshCredential(ctx context.Context, account *domain.Account) error
}

type Runner struct {
	store     *store.Store
	providers *provider.Registry
	sched     *scheduler.Scheduler
	log       *slog.Logger

	interval time.Duration
	timeout  time.Duration
	workers  int
}

func New(st *store.Store, providers *provider.Registry, sched *scheduler.Scheduler, log *slog.Logger, interval time.Duration) *Runner {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Runner{
		store:     st,
		providers: providers,
		sched:     sched,
		log:       log,
		interval:  interval,
		timeout:   defaultTimeout,
		workers:   defaultWorkers,
	}
}

func (r *Runner) Run(ctx context.Context) {
	delay := time.NewTimer(15 * time.Second)
	defer delay.Stop()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-delay.C:
			r.tick(ctx)
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	if r.store == nil || r.providers == nil {
		return
	}
	start := time.Now()
	accounts, err := r.store.ListAccounts(ctx, "")
	if err != nil {
		r.warn("token refresh list accounts", "err", err)
		return
	}

	var checked int64
	var okCount int64
	var failCount int64
	sem := make(chan struct{}, r.workerCount())
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !eligible(acc) {
			continue
		}
		prov, ok := r.providers.Get(acc.Provider)
		if !ok {
			continue
		}
		refresher, ok := prov.(CredentialRefresher)
		if !ok {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(acc *domain.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			atomic.AddInt64(&checked, 1)
			refreshCtx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			err := refresher.RefreshCredential(refreshCtx, acc)
			if err == nil {
				atomic.AddInt64(&okCount, 1)
				return
			}
			atomic.AddInt64(&failCount, 1)
			class := scheduler.ClassifyError(0, "", err)
			if class == domain.ErrBanned && r.sched != nil {
				r.sched.MarkFailure(acc.ID, domain.ErrBanned)
			}
			r.warn("token refresh account failed", "account", acc.ID, "provider", acc.Provider, "class", string(class), "err", err)
		}(acc)
	}
	wg.Wait()

	if checked > 0 {
		r.info("token refresh tick", "checked", checked, "ok", okCount, "failed", failCount, "duration", time.Since(start).String())
	}
}

func (r *Runner) workerCount() int {
	if r.workers <= 0 {
		return defaultWorkers
	}
	return r.workers
}

func eligible(acc *domain.Account) bool {
	if acc == nil {
		return false
	}
	switch acc.State {
	case domain.StateBanned, domain.StateDisabled:
		return false
	default:
		return true
	}
}

func (r *Runner) info(msg string, args ...any) {
	if r.log != nil {
		r.log.Info(msg, args...)
	}
}

func (r *Runner) warn(msg string, args ...any) {
	if r.log != nil {
		r.log.Warn(msg, args...)
	}
}
