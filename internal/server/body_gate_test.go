package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/config"
)

func TestReadRequestBodyAllowsLongContextWithinConfiguredLimit(t *testing.T) {
	g := &Gateway{
		cfg: &config.Root{Server: config.Server{
			MaxRequestBytes:         512 << 20,
			RequestBodyMemoryBudget: 512 << 20,
		}},
		bodyGate: newBodyGate(512 << 20),
	}
	payload := bytes.Repeat([]byte("x"), 12<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	got, release, err := g.readRequestBody(rec, req)
	if err != nil {
		t.Fatalf("long context body should be accepted: %v", err)
	}
	defer release()
	if len(got) != len(payload) {
		t.Fatalf("body length changed: got %d want %d", len(got), len(payload))
	}
}

func TestReadAllGatedDoesNotReserveMaxBeforeBytesArrive(t *testing.T) {
	g := &Gateway{bodyGate: newBodyGate(1 << 20)}
	blocking := &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		_, release, err := g.readAllGated(context.Background(), blocking, 512<<20)
		if release != nil {
			release()
		}
		done <- err
	}()

	<-blocking.started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	body, release, err := g.readAllGated(ctx, bytes.NewReader([]byte("ok")), 512<<20)
	if err != nil {
		t.Fatalf("small body should not wait behind idle unknown-length reader: %v", err)
	}
	release()
	if string(body) != "ok" {
		t.Fatalf("body changed: %q", body)
	}

	close(blocking.release)
	if err := <-done; err != nil {
		t.Fatalf("blocking reader should finish cleanly: %v", err)
	}
}

func TestReadAllGatedBackpressuresWhenActualBytesFillBudget(t *testing.T) {
	g := &Gateway{bodyGate: newBodyGate(1 << 20)}
	firstBody := bytes.Repeat([]byte("a"), 1<<20)
	_, firstRelease, err := g.readAllGated(context.Background(), bytes.NewReader(firstBody), 512<<20)
	if err != nil {
		t.Fatalf("first body should fill budget: %v", err)
	}
	defer firstRelease()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, secondRelease, err := g.readAllGated(ctx, bytes.NewReader([]byte("b")), 512<<20)
	if secondRelease != nil {
		secondRelease()
	}
	if err == nil {
		t.Fatal("second body should wait behind actual occupied budget")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline while backpressured, got %v", err)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    bool
}

func (r *blockingReader) Read(_ []byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.started)
	}
	<-r.release
	return 0, io.EOF
}

func TestReadRequestBodyAllowsPayloadAboveLegacySixtyFourMiB(t *testing.T) {
	g := &Gateway{
		cfg: &config.Root{Server: config.Server{
			MaxRequestBytes:         80 << 20,
			RequestBodyMemoryBudget: 80 << 20,
		}},
		bodyGate: newBodyGate(80 << 20),
	}
	payload := bytes.Repeat([]byte("x"), 65<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	got, release, err := g.readRequestBody(rec, req)
	if err != nil {
		t.Fatalf("payload above legacy 64MiB should be accepted: %v", err)
	}
	defer release()
	if len(got) != len(payload) {
		t.Fatalf("body length changed: got %d want %d", len(got), len(payload))
	}
}
