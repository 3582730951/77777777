package scheduler

import (
	"context"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
)

// Prober runs in the background, periodically probing accounts in non-active
// confidence levels. On success it upgrades them to ConfConfirmedAvailable,
// solving the "stuck cooling" failure mode.
type Prober struct {
	sched    *Scheduler
	interval time.Duration
	probe    ProbeFunc
}

func NewProber(sched *Scheduler, interval time.Duration, probe ProbeFunc) *Prober {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Prober{sched: sched, interval: interval, probe: probe}
}

func (p *Prober) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Prober) tick(ctx context.Context) {
	if p.probe == nil {
		return
	}
	candidates := p.collect()
	for _, id := range candidates {
		select {
		case <-ctx.Done():
			return
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := p.probe(probeCtx, id)
		cancel()
		if err == nil {
			p.sched.MarkSuccess(id, 100)
			continue
		}
		if ClassifyError(0, "", err) == domain.ErrBanned {
			p.sched.MarkFailure(id, domain.ErrBanned)
		}
	}
}

func (p *Prober) collect() []string {
	p.sched.mu.RLock()
	defer p.sched.mu.RUnlock()
	out := []string{}
	now := time.Now()
	for id, sl := range p.sched.accounts {
		sl.mu.Lock()
		if !slotSelectableStateLocked(sl) {
			sl.mu.Unlock()
			continue
		}
		shouldProbe := false
		switch sl.Confidence {
		case domain.ConfProbablyExhausted, domain.ConfSuspectedIssue, domain.ConfCooling:
			shouldProbe = true
		}
		// Also probe if breaker open beyond OpenUntil.
		if sl.BreakerState == domain.BreakerOpen && now.After(sl.OpenUntil) {
			shouldProbe = true
		}
		// Aggressive: if ResetAt has passed, probe.
		if !sl.Account.Quota.ShortWindow.ResetAt.IsZero() &&
			now.After(sl.Account.Quota.ShortWindow.ResetAt) {
			shouldProbe = true
		}
		sl.mu.Unlock()
		if shouldProbe {
			out = append(out, id)
		}
	}
	return out
}
