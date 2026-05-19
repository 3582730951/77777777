package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/llm-pool/gateway/internal/store"
)

func TestRemoteChatUsesConfiguredAccount(t *testing.T) {
	cfg := remoteChatTestConfig()
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:       "remote-acc",
		TenantID: "default",
		Provider: "remote",
		State:    domain.StateActive,
		Quota: domain.QuotaState{DiscoveredModels: []domain.ModelCapability{
			{ID: "remote-model", Available: true},
		}},
	}
	if err := st.UpsertAccount(t.Context(), acc, store.AccountSecret{}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := st.SetSetting(t.Context(), store.SettingRemoteChatAccountID, acc.ID); err != nil {
		t.Fatalf("set setting: %v", err)
	}

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(acc)
	reg := provider.NewRegistry()
	cap := &namedCaptureProvider{name: "remote"}
	reg.Register(cap)

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     sched,
		Providers: reg,
		Store:     st,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/remote-chat/completions", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"remote-model"`) {
		t.Fatalf("default model not used: %s", rec.Body.String())
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.calls != 1 || len(cap.accountIDs) != 1 || cap.accountIDs[0] != "remote-acc" {
		t.Fatalf("provider calls=%d accounts=%v", cap.calls, cap.accountIDs)
	}
}

func TestRemoteChatRequiresConfiguredAccount(t *testing.T) {
	cfg := remoteChatTestConfig()
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	reg := provider.NewRegistry()
	reg.Register(&namedCaptureProvider{name: "remote"})

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     scheduler.New(cfg.Scheduler),
		Providers: reg,
		Store:     st,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/remote-chat", strings.NewReader(`{"message":"hello","model":"remote-model"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "remote_chat_not_configured") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestRemoteChatAppliesGroupSystemPrompt(t *testing.T) {
	cfg := remoteChatTestConfig()
	cfg.Groups[0].SystemPrompt = "group-policy"
	cfg.Groups[0].SystemPromptMode = "prepend"

	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:       "remote-acc",
		TenantID: "default",
		Provider: "remote-capture",
		State:    domain.StateActive,
		Quota: domain.QuotaState{DiscoveredModels: []domain.ModelCapability{
			{ID: "remote-model", Available: true},
		}},
	}
	if err := st.UpsertAccount(t.Context(), acc, store.AccountSecret{}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := st.SetSetting(t.Context(), store.SettingRemoteChatAccountID, acc.ID); err != nil {
		t.Fatalf("set setting: %v", err)
	}

	resolver := auth.NewResolver()
	resolver.LoadFromConfig(cfg)
	sched := scheduler.New(cfg.Scheduler)
	sched.Register(acc)
	reg := provider.NewRegistry()
	cap := &remoteChatSystemCaptureProvider{}
	reg.Register(cap)

	gw := NewGateway(Deps{
		Cfg:       cfg,
		Resolver:  resolver,
		Sched:     sched,
		Providers: reg,
		Store:     st,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := chi.NewRouter()
	gw.Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/remote-chat", strings.NewReader(`{"message":"hello","system":"client-base"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	cap.mu.Lock()
	got := cap.req
	cap.mu.Unlock()
	if got == nil {
		t.Fatal("provider did not receive request")
	}
	if got.System != "group-policy\n\nclient-base" {
		t.Fatalf("system prompt not applied: %q", got.System)
	}
}

func TestPreserveAnthropicClaudeCodeShapeForClaudeGroups(t *testing.T) {
	req := &ir.Request{
		OriginalProto:              "anthropic",
		AnthropicSystem:            []byte(`[{"type":"text","text":"native"}]`),
		AnthropicMetadata:          []byte(`{"user_id":"native"}`),
		AnthropicContextManagement: []byte(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`),
	}
	if !preserveAnthropicClaudeCodeShape(req, &domain.Group{Provider: "claude"}) {
		t.Fatal("native Claude Code request shape should be preserved for claude groups")
	}
	if preserveAnthropicClaudeCodeShape(req, &domain.Group{Provider: "chatgpt"}) {
		t.Fatal("non-claude groups should still allow cross-provider normalization")
	}
}

type remoteChatSystemCaptureProvider struct {
	mu  sync.Mutex
	req *ir.Request
}

func (p *remoteChatSystemCaptureProvider) Name() string { return "remote-capture" }

func (p *remoteChatSystemCaptureProvider) Invoke(ctx context.Context, _ *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
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

func (p *remoteChatSystemCaptureProvider) Probe(context.Context, *domain.Account) error { return nil }

func (p *remoteChatSystemCaptureProvider) Discover(context.Context, *domain.Account) (*domain.QuotaState, error) {
	return &domain.QuotaState{}, nil
}

func remoteChatTestConfig() *config.Root {
	cfg := &config.Root{}
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.MaxRequestBytes = 1 << 20
	cfg.Server.RequestBodyMemoryBudget = 1 << 20
	cfg.Server.RateLimitRPM = 300
	cfg.Server.RateLimitBurst = 60
	cfg.Tenants = []config.Tenant{{ID: "default", Name: "Default"}}
	cfg.Groups = []config.Group{{
		ID:         "normal-group",
		TenantID:   "default",
		Provider:   "unused",
		APIKeys:    []string{"sk-test"},
		Models:     []string{"unused-model"},
		AccountIDs: []string{"unused-acc"},
	}}
	return cfg
}
