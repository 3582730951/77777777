package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/scheduler"
)

type fastProvider struct{}

func (fastProvider) Name() string { return "fast" }

func (fastProvider) Invoke(ctx context.Context, _ *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	out := make(chan ir.Event, 3)
	go func() {
		defer close(out)
		out <- ir.Event{Kind: ir.EvTextDelta, Text: "ok"}
		out <- ir.Event{Kind: ir.EvUsage, InputTokens: len(req.Messages), OutputTokens: 1}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
}

func (fastProvider) Probe(context.Context, *domain.Account) error { return nil }

func (fastProvider) Discover(context.Context, *domain.Account) (*domain.QuotaState, error) {
	return &domain.QuotaState{}, nil
}

type captureProvider struct {
	mu  sync.Mutex
	req *ir.Request
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) Invoke(ctx context.Context, _ *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	p.mu.Lock()
	cp := *req
	p.req = &cp
	p.mu.Unlock()
	out := make(chan ir.Event, 2)
	go func() {
		defer close(out)
		out <- ir.Event{Kind: ir.EvTextDelta, Text: "ok"}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
}

func (p *captureProvider) Probe(context.Context, *domain.Account) error { return nil }

func (p *captureProvider) Discover(context.Context, *domain.Account) (*domain.QuotaState, error) {
	return &domain.QuotaState{}, nil
}

type failingProvider struct {
	err error
}

func (p failingProvider) Name() string { return "failing" }

func (p failingProvider) Invoke(context.Context, *domain.Account, *ir.Request) (<-chan ir.Event, error) {
	return nil, p.err
}

func (p failingProvider) Probe(context.Context, *domain.Account) error { return p.err }

func (p failingProvider) Discover(context.Context, *domain.Account) (*domain.QuotaState, error) {
	return nil, p.err
}

func TestGatewayAllowsFiftyConcurrentRequestsForOneCLIKey(t *testing.T) {
	cfg := &config.Root{}
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.MaxRequestBytes = 512 << 20
	cfg.Server.RequestBodyMemoryBudget = 512 << 20
	cfg.Server.RateLimitRPM = 300
	cfg.Server.RateLimitBurst = 60
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.NeverFail.MaxWait = time.Second
	cfg.Scheduler.Sticky.Enabled = true
	cfg.Tenants = []config.Tenant{{ID: "default", Name: "Default"}}
	cfg.Groups = []config.Group{{
		ID:             "g",
		TenantID:       "default",
		Provider:       "fast",
		APIKeys:        []string{"sk-test"},
		Models:         []string{"model-high"},
		ModelWhitelist: []string{"model-high"},
		AccountIDs:     []string{"acc-1"},
	}}

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "fast",
		State:    domain.StateActive,
	})
	providers := provider.NewRegistry()
	providers.Register(fastProvider{})

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     sched,
		Providers: providers,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	var wg sync.WaitGroup
	statuses := make(chan int, 50)
	body := `{"model":"model-high","messages":[{"role":"user","content":"write a long file"}],"stream":false,"reasoning_effort":"high","max_tokens":4096}`
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer sk-test")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)

	for status := range statuses {
		if status == http.StatusTooManyRequests {
			t.Fatal("50 concurrent CLI starts must not be rejected by local gateway rate limit")
		}
		if status != http.StatusOK {
			t.Fatalf("unexpected status: %d", status)
		}
	}
}

func TestGatewayDoesNotLowerModelReasoningOrMaxTokens(t *testing.T) {
	cfg := &config.Root{}
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.MaxRequestBytes = 512 << 20
	cfg.Server.RequestBodyMemoryBudget = 512 << 20
	cfg.Server.RateLimitRPM = 300
	cfg.Server.RateLimitBurst = 60
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.NeverFail.MaxWait = time.Second
	cfg.Tenants = []config.Tenant{{ID: "default", Name: "Default"}}
	cfg.Groups = []config.Group{{
		ID:             "g",
		TenantID:       "default",
		Provider:       "capture",
		APIKeys:        []string{"sk-test"},
		Models:         []string{"model-high"},
		ModelWhitelist: []string{"model-high"},
		AccountIDs:     []string{"acc-1"},
	}}

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "capture",
		State:    domain.StateActive,
	})
	providers := provider.NewRegistry()
	cap := &captureProvider{}
	providers.Register(cap)

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     sched,
		Providers: providers,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	body := `{"model":"model-high","messages":[{"role":"user","content":"preserve long reasoning quality"}],"stream":false,"reasoning_effort":"high","max_tokens":4096}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	cap.mu.Lock()
	got := cap.req
	cap.mu.Unlock()
	if got == nil {
		t.Fatal("provider did not receive request")
	}
	if got.Model != "model-high" {
		t.Fatalf("model was changed: %q", got.Model)
	}
	if got.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort was changed: %q", got.ReasoningEffort)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("max_tokens was changed: %d", got.MaxTokens)
	}
}

func TestGatewayMarksBannedAccountFromInvokeError(t *testing.T) {
	cfg := &config.Root{}
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.MaxRequestBytes = 512 << 20
	cfg.Server.RequestBodyMemoryBudget = 512 << 20
	cfg.Server.RateLimitRPM = 300
	cfg.Server.RateLimitBurst = 60
	cfg.Scheduler.Retry.MaxAttempts = 1
	cfg.Scheduler.Failover.NeverFail.MaxWait = time.Second
	cfg.Scheduler.Failover.NeverFail.Enabled = false
	cfg.Tenants = []config.Tenant{{ID: "default", Name: "Default"}}
	cfg.Groups = []config.Group{{
		ID:             "g",
		TenantID:       "default",
		Provider:       "failing",
		APIKeys:        []string{"sk-test"},
		Models:         []string{"model-high"},
		ModelWhitelist: []string{"model-high"},
		AccountIDs:     []string{"acc-1"},
	}}

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(&domain.Account{
		ID:       "acc-1",
		TenantID: "default",
		Provider: "failing",
		State:    domain.StateActive,
	})
	providers := provider.NewRegistry()
	providers.Register(failingProvider{err: errors.New("account_disabled: account is disabled")})

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     sched,
		Providers: providers,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	body := `{"model":"model-high","messages":[{"role":"user","content":"test"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	acc, ok := sched.AccountByID("acc-1")
	if !ok {
		t.Fatal("account missing")
	}
	if acc.State != domain.StateBanned {
		t.Fatalf("invoke banned signal should mark account banned, got %s", acc.State)
	}
	_, err := sched.Pick(context.Background(), scheduler.PickRequest{Provider: "failing", TenantID: "default"})
	if err == nil {
		t.Fatal("banned account should not remain schedulable")
	}
}
