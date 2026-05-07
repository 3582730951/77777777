// Package stream implements the three-phase failover state machine described
// in plan §八点六. Phase A (pre-flight): full silent retry. Phase B (head buffer):
// hold first 500ms / 16KB / 5 events of upstream output; if upstream errors
// before flush, retry on a different account silently. Phase C (live stream):
// best-effort graceful close on mid-stream errors so downstream SDKs never see
// a broken connection.
package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

type HeadBufferConfig struct {
	MaxDuration time.Duration
	MaxBytes    int
	MaxEvents   int
}

func (c HeadBufferConfig) sane() HeadBufferConfig {
	if c.MaxDuration <= 0 {
		c.MaxDuration = 500 * time.Millisecond
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 16 * 1024
	}
	if c.MaxEvents <= 0 {
		c.MaxEvents = 5
	}
	return c
}

// AttemptResult is what BufferHead returns after the buffering window closes.
type AttemptResult int

const (
	// AttemptCommitted: the head buffer flushed successfully, callers must
	// continue piping events to the downstream encoder.
	AttemptCommitted AttemptResult = iota
	// AttemptUpstreamError: upstream errored within the head window. The
	// buffered events were discarded; caller should silently retry.
	AttemptUpstreamError
	// AttemptDownstreamGone: the downstream client disconnected before
	// commit; abandon.
	AttemptDownstreamGone
)

// BufferHead drains the upstream channel for at most the configured window and
// either commits (re-emitting buffered events to the downstream sink and then
// passing through subsequent events) or signals retry.
//
// commit is invoked once per buffered event in commit-order, then for every
// subsequent live event. If commit returns an error the stream is aborted.
type BufferHead struct {
	cfg HeadBufferConfig
}

func NewBufferHead(cfg HeadBufferConfig) *BufferHead {
	return &BufferHead{cfg: cfg.sane()}
}

// Run executes the head-buffer state machine. It returns once either:
//   - the buffered window completed without an upstream error (events committed
//     and remaining events piped),
//   - upstream channel closed inside the window with an error (signals retry),
//   - downstream context was canceled.
func (b *BufferHead) Run(
	ctx context.Context,
	upstream <-chan ir.Event,
	commit func(ir.Event) error,
) (AttemptResult, error) {
	timer := time.NewTimer(b.cfg.MaxDuration)
	defer timer.Stop()

	buf := make([]ir.Event, 0, b.cfg.MaxEvents+1)
	bytes := 0
	committed := false

	flush := func() error {
		for _, ev := range buf {
			if err := commit(ev); err != nil {
				return err
			}
		}
		buf = nil
		committed = true
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return AttemptDownstreamGone, ctx.Err()

		case ev, ok := <-upstream:
			if !ok {
				if !committed {
					// Channel closed cleanly with no error. Emit buffered.
					if err := flush(); err != nil {
						return AttemptDownstreamGone, err
					}
				}
				return AttemptCommitted, nil
			}
			if ev.Kind == ir.EvError {
				if committed {
					return AttemptCommitted, ev.Err
				}
				return AttemptUpstreamError, ev.Err
			}
			if !committed {
				buf = append(buf, ev)
				bytes += approxSize(ev)
				if len(buf) >= b.cfg.MaxEvents || bytes >= b.cfg.MaxBytes {
					if err := flush(); err != nil {
						return AttemptDownstreamGone, err
					}
					// Continue piping remaining events.
					continue
				}
			} else {
				if err := commit(ev); err != nil {
					return AttemptDownstreamGone, err
				}
			}

		case <-timer.C:
			if !committed {
				if err := flush(); err != nil {
					return AttemptDownstreamGone, err
				}
			}
			// Continue draining live.
			for {
				select {
				case <-ctx.Done():
					return AttemptDownstreamGone, ctx.Err()
				case ev, ok := <-upstream:
					if !ok {
						return AttemptCommitted, nil
					}
					if ev.Kind == ir.EvError {
						return AttemptCommitted, ev.Err
					}
					if err := commit(ev); err != nil {
						return AttemptDownstreamGone, err
					}
				}
			}
		}
	}
}

func approxSize(ev ir.Event) int {
	return len(ev.Text) + len(ev.ToolDelta) + len(ev.ToolID) + len(ev.ToolName)
}

// MultiplexInvoker chains attempts: upstream-attempts are produced by the
// caller's makeAttempt closure; the first that commits wins. If all retries
// exhaust without committing, error is returned and the gateway is expected to
// emit a graceful EvDone to downstream (best_effort_close).
type MultiplexInvoker struct {
	HeadBuffer  HeadBufferConfig
	MaxAttempts int
}

func (m *MultiplexInvoker) Run(
	ctx context.Context,
	makeAttempt func(attempt int) (<-chan ir.Event, func(), error),
	commit func(ir.Event) error,
) error {
	bh := NewBufferHead(m.HeadBuffer)
	maxAttempts := m.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ch, release, err := makeAttempt(attempt)
		if err != nil {
			lastErr = err
			continue
		}
		var sync_ sync.Once
		release = func() func() {
			r := release
			return func() { sync_.Do(r) }
		}()
		result, runErr := bh.Run(ctx, ch, commit)
		release()
		switch result {
		case AttemptCommitted:
			return runErr
		case AttemptUpstreamError:
			lastErr = runErr
			continue
		case AttemptDownstreamGone:
			return runErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all attempts exhausted")
	}
	return lastErr
}
