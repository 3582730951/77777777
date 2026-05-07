package server

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

func TestByteRateLimiterDelaysWithoutChangingPayload(t *testing.T) {
	limiter := newByteRateLimiter(64<<10, 32<<10)
	payload := bytes.Repeat([]byte("x"), 96<<10)
	reader := &rateLimitedReader{
		Reader:  bytes.NewReader(payload),
		ctx:     context.Background(),
		limiter: limiter,
	}

	start := time.Now()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read through limiter: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("network shaper changed payload")
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("expected ingress backpressure delay, got %s", time.Since(start))
	}
}

func TestShapedResponseWriterDoesNotChangePayload(t *testing.T) {
	limiter := newByteRateLimiter(64<<10, 32<<10)
	payload := bytes.Repeat([]byte("y"), 96<<10)
	rec := httptest.NewRecorder()
	w := &shapedResponseWriter{
		ResponseWriter: rec,
		ctx:            context.Background(),
		limiter:        limiter,
	}

	start := time.Now()
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("write through limiter: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("short write: got %d want %d", n, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("network shaper changed response payload")
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("expected egress backpressure delay, got %s", time.Since(start))
	}
}
