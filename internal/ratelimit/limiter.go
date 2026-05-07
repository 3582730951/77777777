// Package ratelimit implements gateway-level per-API-key rate limiting
// using a token bucket algorithm with automatic cleanup.
package ratelimit

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Limiter enforces per-key request rate limits at the gateway level.
// This is distinct from upstream provider quota tracking — it protects
// the gateway itself from abusive clients.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rpm     float64 // requests per minute
	burst   int
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a limiter that allows `rpm` requests per minute with burst capacity.
func New(rpm float64, burst int) *Limiter {
	if rpm <= 0 {
		rpm = 60
	}
	if burst <= 0 {
		burst = int(rpm)
	}
	l := &Limiter{
		buckets: make(map[string]*bucket),
		rpm:     rpm,
		burst:   burst,
	}
	go l.cleanup()
	return l
}

// Allow checks if a request from the given key is allowed.
// Returns true if allowed, false if rate limited.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), lastSeen: now}
		l.buckets[key] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rpm / 60.0
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Wait blocks until a request from key can proceed. It applies backpressure
// instead of rejecting downstream CLI clients with 429.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	for {
		delay := l.delayUntilAllowed(key)
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (l *Limiter) delayUntilAllowed(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), lastSeen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rpm / 60.0
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	needed := 1 - b.tokens
	return time.Duration(needed * 60.0 / l.rpm * float64(time.Second))
}

// Middleware returns an HTTP middleware that enforces rate limiting.
// The keyFunc extracts the rate-limit key from the request (typically the API key).
func (l *Limiter) Middleware(keyFunc func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFunc(r)
			if err := l.Wait(r.Context(), key); err != nil {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// cleanup removes stale buckets every 5 minutes.
func (l *Limiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		l.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for k, b := range l.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}
