// Package audit — sampler uses async ring-buffer batch writes to SQLite.
// See doc comment in sampler_async.go.
package audit

import (
	"context"
	"time"

	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

const (
	sampleRingCap    = 4096
	sampleFlushEvery = 5 * time.Second
)

type quotaSampleMsg struct {
	accountID string
	sample    store.QuotaSample
}

// Sampler periodically snapshots scheduler state into store.quota_samples.
// Writes are batched asynchronously via a ring buffer to avoid serialising
// on the SQLite WAL lock at high concurrency.
type Sampler struct {
	sched  *scheduler.Scheduler
	store  *store.Store
	period time.Duration
	ring   chan quotaSampleMsg
}

func NewSampler(sched *scheduler.Scheduler, st *store.Store, period time.Duration) *Sampler {
	if period <= 0 {
		period = 60 * time.Second
	}
	return &Sampler{
		sched:  sched,
		store:  st,
		period: period,
		ring:   make(chan quotaSampleMsg, sampleRingCap),
	}
}

func (s *Sampler) Run(ctx context.Context) {
	go s.flusher(ctx)

	ticker := time.NewTicker(s.period)
	defer ticker.Stop()
	prune := time.NewTicker(15 * time.Minute)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		case <-prune.C:
			go func() {
				ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = s.store.PruneQuotaSamples(ctx2, 7*24*time.Hour)
				_ = s.store.PruneRequestSamples(ctx2, 7*24*time.Hour)
			}()
		}
	}
}

func (s *Sampler) tick() {
	now := time.Now()
	for _, sl := range s.sched.Snapshot() {
		msg := quotaSampleMsg{
			accountID: sl.AccountID,
			sample: store.QuotaSample{
				At:         now,
				EWMAMs:     sl.EWMALatency,
				Inflight:   sl.Inflight,
				Confidence: sl.Confidence,
				Breaker:    sl.BreakerState,
			},
		}
		// Non-blocking enqueue; drop if ring full (metrics lag is acceptable).
		select {
		case s.ring <- msg:
		default:
			select { case <-s.ring: default: }
			select { case s.ring <- msg: default: }
		}
	}
}

func (s *Sampler) flusher(ctx context.Context) {
	ticker := time.NewTicker(sampleFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushBatch(context.Background())
			return
		case <-ticker.C:
			s.flushBatch(ctx)
		}
	}
}

func (s *Sampler) flushBatch(ctx context.Context) {
	const maxPerFlush = sampleRingCap / 2
	for i := 0; i < maxPerFlush; i++ {
		select {
		case msg := <-s.ring:
			_ = s.store.AppendQuotaSample(ctx, msg.accountID, msg.sample)
		default:
			return
		}
	}
}
