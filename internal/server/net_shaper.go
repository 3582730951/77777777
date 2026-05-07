package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

const networkShapeChunk = 32 << 10

type byteRateLimiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	tokens   float64
	lastSeen time.Time
}

func newByteRateLimiter(rateBytesPerSec, burstBytes int64) *byteRateLimiter {
	if rateBytesPerSec <= 0 {
		return nil
	}
	if burstBytes <= 0 {
		burstBytes = rateBytesPerSec
	}
	if burstBytes < networkShapeChunk {
		burstBytes = networkShapeChunk
	}
	return &byteRateLimiter{
		rate:     float64(rateBytesPerSec),
		burst:    float64(burstBytes),
		tokens:   float64(burstBytes),
		lastSeen: time.Now(),
	}
}

func (l *byteRateLimiter) maxChunk() int {
	if l == nil {
		return networkShapeChunk
	}
	if l.burst < networkShapeChunk {
		return int(l.burst)
	}
	return networkShapeChunk
}

func (l *byteRateLimiter) wait(ctx context.Context, bytes int) error {
	if l == nil || bytes <= 0 {
		return nil
	}
	remaining := bytes
	for remaining > 0 {
		n := remaining
		if maxChunk := l.maxChunk(); n > maxChunk {
			n = maxChunk
		}
		if err := l.waitChunk(ctx, n); err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

func (l *byteRateLimiter) waitChunk(ctx context.Context, bytes int) error {
	if bytes <= 0 {
		return nil
	}
	delay := l.reserve(bytes)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	}
}

func (l *byteRateLimiter) reserve(bytes int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastSeen).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.lastSeen = now
	}

	need := float64(bytes)
	l.tokens -= need
	if l.tokens >= 0 {
		return 0
	}
	seconds := -l.tokens / l.rate
	return time.Duration(math.Ceil(seconds * float64(time.Second)))
}

type rateLimitedReadCloser struct {
	io.ReadCloser
	ctx     context.Context
	limiter *byteRateLimiter
}

func (r *rateLimitedReadCloser) Read(p []byte) (int, error) {
	if r.limiter == nil {
		return r.ReadCloser.Read(p)
	}
	if maxChunk := r.limiter.maxChunk(); len(p) > maxChunk {
		p = p[:maxChunk]
	}
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		if waitErr := r.limiter.wait(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

type rateLimitedReader struct {
	io.Reader
	ctx     context.Context
	limiter *byteRateLimiter
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	if r.limiter == nil {
		return r.Reader.Read(p)
	}
	if maxChunk := r.limiter.maxChunk(); len(p) > maxChunk {
		p = p[:maxChunk]
	}
	n, err := r.Reader.Read(p)
	if n > 0 {
		if waitErr := r.limiter.wait(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

type shapedResponseWriter struct {
	http.ResponseWriter
	ctx     context.Context
	limiter *byteRateLimiter
}

func (w *shapedResponseWriter) Write(p []byte) (int, error) {
	if w.limiter == nil || len(p) == 0 {
		return w.ResponseWriter.Write(p)
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if maxChunk := w.limiter.maxChunk(); len(chunk) > maxChunk {
			chunk = chunk[:maxChunk]
		}
		if err := w.limiter.wait(w.ctx, len(chunk)); err != nil {
			return total, err
		}
		n, err := w.ResponseWriter.Write(chunk)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (w *shapedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *shapedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	return h.Hijack()
}

func (w *shapedResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *shapedResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(w, r)
}

func (w *shapedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
