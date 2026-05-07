package audit

import (
	"context"
	"sync"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/scheduler"
)

// DiscoveryRunner periodically calls each provider's Discover() to refresh the
// account's model list and quota windows. Frequency adapts: stable accounts
// poll less, recently rate-limited accounts poll more (plan §八点七 表).
type DiscoveryRunner struct {
	cfg          config.Discovery
	sched        *scheduler.Scheduler
	discoverFunc func(ctx context.Context, accountID string) (*domain.QuotaState, error)
	mu           sync.Mutex
	last         map[string]time.Time
	stableSince  map[string]int // consecutive unchanged samples
}

func NewDiscoveryRunner(cfg config.Discovery, sched *scheduler.Scheduler,
	df func(ctx context.Context, accountID string) (*domain.QuotaState, error)) *DiscoveryRunner {
	if cfg.DefaultInterval <= 0 {
		cfg.DefaultInterval = 5 * time.Minute
	}
	if cfg.StableInterval <= 0 {
		cfg.StableInterval = 15 * time.Minute
	}
	if cfg.UnstableInterval <= 0 {
		cfg.UnstableInterval = 1 * time.Minute
	}
	return &DiscoveryRunner{
		cfg: cfg, sched: sched, discoverFunc: df,
		last:        map[string]time.Time{},
		stableSince: map[string]int{},
	}
}

func (r *DiscoveryRunner) Run(ctx context.Context) {
	// Run discovery immediately on startup, then every 30s.
	r.tick(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *DiscoveryRunner) tick(ctx context.Context) {
	if r.discoverFunc == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	last := make(map[string]time.Time, len(r.last))
	for k, v := range r.last {
		last[k] = v
	}
	stable := make(map[string]int, len(r.stableSince))
	for k, v := range r.stableSince {
		stable[k] = v
	}
	r.mu.Unlock()

	for _, sl := range r.sched.Snapshot() {
		id := sl.AccountID
		interval := r.cfg.DefaultInterval
		if sl.Confidence == "suspected_issue" || sl.Confidence == "probably_exhausted" {
			interval = r.cfg.UnstableInterval
		} else if stable[id] >= 5 {
			interval = r.cfg.StableInterval
		}
		if t, ok := last[id]; ok && now.Sub(t) < interval {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		state, err := r.discoverFunc(probeCtx, id)
		cancel()
		r.mu.Lock()
		r.last[id] = now
		if err == nil && state != nil {
			r.stableSince[id]++
			r.sched.UpdateQuota(id, state)
		} else {
			r.stableSince[id] = 0
			if err != nil {
				if scheduler.ClassifyError(0, "", err) == domain.ErrBanned {
					r.sched.MarkFailure(id, domain.ErrBanned)
				}
			}
		}
		r.mu.Unlock()
	}
}
