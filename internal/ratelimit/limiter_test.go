package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestDefaultBurstAllowsFiftyConcurrentStarts(t *testing.T) {
	l := New(300, 60)
	for i := 0; i < 50; i++ {
		if !l.Allow("key") {
			t.Fatalf("request %d should be allowed by local gateway burst", i+1)
		}
	}
}

func TestLegacyBurstWouldNotMeetFiftyConcurrentStarts(t *testing.T) {
	l := New(120, 20)
	allowed := 0
	for i := 0; i < 50; i++ {
		if l.Allow("key") {
			allowed++
		}
	}
	if allowed >= 50 {
		t.Fatalf("test expects burst 20 to reject some starts, allowed=%d", allowed)
	}
}

func TestMiddlewareBackpressuresInsteadOfRejecting(t *testing.T) {
	l := New(600, 1)
	if !l.Allow("key") {
		t.Fatal("first request should consume burst token")
	}
	start := time.Now()
	if err := l.Wait(context.Background(), "key"); err != nil {
		t.Fatalf("wait should not reject: %v", err)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Fatalf("second request should be delayed by backpressure, waited %s", time.Since(start))
	}
}
