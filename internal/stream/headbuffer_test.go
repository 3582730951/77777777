package stream_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/stream"
)

func TestHeadBufferSilentRetry(t *testing.T) {
	mux := &stream.MultiplexInvoker{
		HeadBuffer:  stream.HeadBufferConfig{MaxDuration: 200 * time.Millisecond, MaxBytes: 1024, MaxEvents: 5},
		MaxAttempts: 3,
	}
	var committed []string
	commit := func(ev ir.Event) error {
		if ev.Kind == ir.EvTextDelta {
			committed = append(committed, ev.Text)
		}
		return nil
	}
	var attempts int32
	makeAttempt := func(attempt int) (<-chan ir.Event, func(), error) {
		atomic.AddInt32(&attempts, 1)
		ch := make(chan ir.Event, 4)
		if attempt == 0 {
			// First attempt: error inside head window.
			go func() {
				ch <- ir.Event{Kind: ir.EvError, Err: errors.New("upstream 429")}
				close(ch)
			}()
		} else {
			// Retry: succeed cleanly.
			go func() {
				ch <- ir.Event{Kind: ir.EvTextDelta, Text: "Hi"}
				ch <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
				close(ch)
			}()
		}
		return ch, func() {}, nil
	}
	if err := mux.Run(context.Background(), makeAttempt, commit); err != nil {
		t.Fatalf("mux: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts (silent retry), got %d", atomic.LoadInt32(&attempts))
	}
	if len(committed) == 0 || committed[0] != "Hi" {
		t.Errorf("downstream did not receive retry payload: %v", committed)
	}
}

func TestHeadBufferCommitsAfterTimeout(t *testing.T) {
	mux := &stream.MultiplexInvoker{
		HeadBuffer:  stream.HeadBufferConfig{MaxDuration: 50 * time.Millisecond, MaxBytes: 1024 * 1024, MaxEvents: 100},
		MaxAttempts: 1,
	}
	var got []string
	commit := func(ev ir.Event) error {
		if ev.Kind == ir.EvTextDelta {
			got = append(got, ev.Text)
		}
		return nil
	}
	makeAttempt := func(attempt int) (<-chan ir.Event, func(), error) {
		ch := make(chan ir.Event, 10)
		go func() {
			ch <- ir.Event{Kind: ir.EvTextDelta, Text: "first"}
			time.Sleep(80 * time.Millisecond) // crosses timeout boundary
			ch <- ir.Event{Kind: ir.EvTextDelta, Text: "second"}
			ch <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
			close(ch)
		}()
		return ch, func() {}, nil
	}
	if err := mux.Run(context.Background(), makeAttempt, commit); err != nil {
		t.Fatalf("mux: %v", err)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("expected ordered passthrough, got %v", got)
	}
}
