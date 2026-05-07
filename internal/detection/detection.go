// Package detection tracks per-account anti-bot signals (CF challenges,
// repeated 403s, persona-bound failures) and auto-quarantines accounts whose
// challenge frequency crosses thresholds. This implements §七 L5 of the plan.
package detection

import (
	"context"
	"sync"
	"time"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/scheduler"
)

type Loop struct {
	mu        sync.Mutex
	events    map[string][]event
	threshold int
	window    time.Duration
	cooloff   time.Duration
	sched     *scheduler.Scheduler
	audit     *audit.Logger
}

type event struct {
	at   time.Time
	kind string // "cf" / "auth" / "rate"
}

func New(sched *scheduler.Scheduler, auditL *audit.Logger) *Loop {
	return &Loop{
		events:    map[string][]event{},
		threshold: 3,
		window:    24 * time.Hour,
		cooloff:   2 * time.Hour,
		sched:     sched,
		audit:     auditL,
	}
}

// RecordChallenge feeds a CF challenge / detection event into the loop.
// If the account crosses the threshold, it is quarantined for `cooloff`.
func (l *Loop) RecordChallenge(accountID, kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	evs := l.events[accountID]
	// trim
	keep := evs[:0]
	for _, e := range evs {
		if e.at.After(cutoff) {
			keep = append(keep, e)
		}
	}
	keep = append(keep, event{at: now, kind: kind})
	l.events[accountID] = keep

	if len(keep) >= l.threshold {
		// Quarantine: open the breaker for cooloff period.
		// The scheduler doesn't expose direct breaker injection, so we simulate
		// by calling MarkFailure repeatedly with the failure threshold.
		for i := 0; i < 10; i++ {
			l.sched.MarkFailure(accountID, domain.ErrCFChallenge)
		}
		if l.audit != nil {
			l.audit.Log("warn", "detection", accountID, "",
				"persona auto-quarantined: "+kind+" events="+itoa(len(keep)))
		}
		// reset events so we don't re-quarantine immediately on the next probe
		l.events[accountID] = nil
	}
}

// Run periodically gc's old events and looks for ResetAt-expired quarantines
// (the prober already handles wake-up).
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.gc()
		}
	}
}

func (l *Loop) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	for k, evs := range l.events {
		keep := evs[:0]
		for _, e := range evs {
			if e.at.After(cutoff) {
				keep = append(keep, e)
			}
		}
		if len(keep) == 0 {
			delete(l.events, k)
		} else {
			l.events[k] = keep
		}
	}
}

// Stats returns per-account challenge counts in the rolling window.
func (l *Loop) Stats() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for k, v := range l.events {
		out[k] = len(v)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
