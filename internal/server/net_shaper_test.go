package server

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/config"
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

func TestGatewayNetworkShaperCanBeDisabledWithZeroConfig(t *testing.T) {
	gw := NewGateway(Deps{Cfg: &config.Root{}})
	if gw.ingressLimiter != nil {
		t.Fatal("ingress limiter should be disabled when configured rate is 0")
	}
	if gw.egressLimiter != nil {
		t.Fatal("egress limiter should be disabled when configured rate is 0")
	}
}

func TestGatewayApplyNetworkShapingConfigUpdatesRuntimeLimiters(t *testing.T) {
	cfg := &config.Root{}
	cfg.Server.NetworkIngressBytesPerSec = 64 << 10
	cfg.Server.NetworkEgressBytesPerSec = 64 << 10
	cfg.Server.NetworkBurstBytes = 32 << 10
	gw := NewGateway(Deps{Cfg: cfg})
	if gw.currentIngressLimiter() == nil || gw.currentEgressLimiter() == nil {
		t.Fatal("expected initial runtime limiters")
	}

	gw.ApplyNetworkShapingConfig(config.Server{
		NetworkIngressBytesPerSec: 0,
		NetworkEgressBytesPerSec:  0,
		NetworkBurstBytes:         0,
	})
	if gw.currentIngressLimiter() != nil {
		t.Fatal("ingress limiter should be disabled after runtime update")
	}
	if gw.currentEgressLimiter() != nil {
		t.Fatal("egress limiter should be disabled after runtime update")
	}
	if cfg.Server.NetworkIngressBytesPerSec != 0 || cfg.Server.NetworkEgressBytesPerSec != 0 {
		t.Fatalf("runtime config not updated: %+v", cfg.Server)
	}

	gw.ApplyNetworkShapingConfig(config.Server{
		NetworkIngressBytesPerSec: 128 << 10,
		NetworkEgressBytesPerSec:  256 << 10,
		NetworkBurstBytes:         32 << 10,
	})
	if gw.currentIngressLimiter() == nil || gw.currentEgressLimiter() == nil {
		t.Fatal("limiters should be re-enabled after runtime update")
	}
	if cfg.Server.NetworkIngressBytesPerSec != 128<<10 || cfg.Server.NetworkEgressBytesPerSec != 256<<10 {
		t.Fatalf("runtime config not updated after re-enable: %+v", cfg.Server)
	}
}
