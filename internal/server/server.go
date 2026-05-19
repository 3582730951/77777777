// Package server wires the inbound HTTP layer: routes, auth, protocol decode,
// model rewriting, scheduling, provider invocation, head-buffered failover,
// protocol encode. /metrics, /healthz and admin routes are mounted alongside.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/cacheopt"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/conversation"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/metrics"
	"github.com/llm-pool/gateway/internal/normalize"
	"github.com/llm-pool/gateway/internal/protocol/anthropic"
	geminiproto "github.com/llm-pool/gateway/internal/protocol/gemini"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/protocol/openai"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/ratelimit"
	"github.com/llm-pool/gateway/internal/responsesstate"
	"github.com/llm-pool/gateway/internal/router"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/stealth"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/stream"
)

type Gateway struct {
	cfg            *config.Root
	resolver       *auth.Resolver
	sched          *scheduler.Scheduler
	providers      *provider.Registry
	logger         *slog.Logger
	audit          *audit.Logger
	store          *store.Store
	convMap        *conversation.Mapper
	responses      *responsesstate.Store
	limiter        *ratelimit.Limiter
	bodyGate       *bodyGate
	networkMu      sync.RWMutex
	ingressLimiter *byteRateLimiter
	egressLimiter  *byteRateLimiter
	identity       *stealth.IdentityStore
	// QuotaRefreshFunc is called asynchronously after each request to push
	// provider-cached quota data into the scheduler. Set by main.go.
	QuotaRefreshFunc func(accountID string)
}

type Deps struct {
	Cfg       *config.Root
	Resolver  *auth.Resolver
	Sched     *scheduler.Scheduler
	Providers *provider.Registry
	Logger    *slog.Logger
	Audit     *audit.Logger
	Store     *store.Store
	ConvMap   *conversation.Mapper
}

func NewGateway(d Deps) *Gateway {
	cm := d.ConvMap
	if cm == nil {
		cm = conversation.New(10000, 30*time.Minute)
	}
	responsesLimit := 10000
	responsesBytes := int64(256 << 20)
	responsesTTL := 12 * time.Hour
	rpm := float64(300)
	burst := 60
	bodyBudget := int64(256 << 20)
	ingressRate := int64(8 << 20)
	egressRate := int64(4 << 20)
	networkBurst := int64(1 << 20)
	if d.Cfg != nil {
		if d.Cfg.Resource.ResponsesStateMax > 0 {
			responsesLimit = d.Cfg.Resource.ResponsesStateMax
		}
		if d.Cfg.Resource.ResponsesStateBytes > 0 {
			responsesBytes = d.Cfg.Resource.ResponsesStateBytes
		}
		if d.Cfg.Resource.ResponsesStateTTL > 0 {
			responsesTTL = d.Cfg.Resource.ResponsesStateTTL
		}
		if d.Cfg.Server.RateLimitRPM > 0 {
			rpm = d.Cfg.Server.RateLimitRPM
		}
		if d.Cfg.Server.RateLimitBurst > 0 {
			burst = d.Cfg.Server.RateLimitBurst
		}
		if d.Cfg.Server.RequestBodyMemoryBudget > 0 {
			bodyBudget = d.Cfg.Server.RequestBodyMemoryBudget
		}
		// Network shapers intentionally honor explicit 0 values so operators can
		// disable byte-rate shaping for latency-sensitive streaming.
		ingressRate = d.Cfg.Server.NetworkIngressBytesPerSec
		egressRate = d.Cfg.Server.NetworkEgressBytesPerSec
		networkBurst = d.Cfg.Server.NetworkBurstBytes
	}
	var identity *stealth.IdentityStore
	if d.Cfg != nil && d.Cfg.Stealth.IdentityRewrite {
		identity = stealth.NewIdentityStore(d.Cfg.Stealth.IdentityPath)
	}
	return &Gateway{
		cfg:            d.Cfg,
		resolver:       d.Resolver,
		sched:          d.Sched,
		providers:      d.Providers,
		logger:         d.Logger,
		audit:          d.Audit,
		store:          d.Store,
		convMap:        cm,
		responses:      responsesstate.NewWithByteLimit(responsesLimit, responsesTTL, responsesBytes),
		limiter:        ratelimit.New(rpm, burst),
		bodyGate:       newBodyGate(bodyBudget),
		ingressLimiter: newByteRateLimiter(ingressRate, networkBurst),
		egressLimiter:  newByteRateLimiter(egressRate, networkBurst),
		identity:       identity,
	}
}

// ConvMap exposes the mapper so admin handlers can read live in-memory stats.
func (g *Gateway) ConvMap() *conversation.Mapper { return g.convMap }

// ApplyNetworkShapingConfig updates byte-rate shaping without rebuilding the gateway.
// A 0 ingress or egress rate disables that direction.
func (g *Gateway) ApplyNetworkShapingConfig(serverCfg config.Server) {
	if g == nil {
		return
	}
	g.networkMu.Lock()
	defer g.networkMu.Unlock()
	if g.cfg != nil {
		g.cfg.Server.NetworkIngressBytesPerSec = serverCfg.NetworkIngressBytesPerSec
		g.cfg.Server.NetworkEgressBytesPerSec = serverCfg.NetworkEgressBytesPerSec
		g.cfg.Server.NetworkBurstBytes = serverCfg.NetworkBurstBytes
	}
	g.ingressLimiter = newByteRateLimiter(serverCfg.NetworkIngressBytesPerSec, serverCfg.NetworkBurstBytes)
	g.egressLimiter = newByteRateLimiter(serverCfg.NetworkEgressBytesPerSec, serverCfg.NetworkBurstBytes)
}

func (g *Gateway) currentIngressLimiter() *byteRateLimiter {
	if g == nil {
		return nil
	}
	g.networkMu.RLock()
	defer g.networkMu.RUnlock()
	return g.ingressLimiter
}

func (g *Gateway) currentEgressLimiter() *byteRateLimiter {
	if g == nil {
		return nil
	}
	g.networkMu.RLock()
	defer g.networkMu.RUnlock()
	return g.egressLimiter
}

func (g *Gateway) Mount(r chi.Router) {
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(responseHeaderHygieneMiddleware)
	r.Use(g.networkMiddleware)
	r.Use(middleware.Compress(5, "application/json"))
	r.Use(corsMiddleware)
	r.Use(g.requestLogger)
	r.Use(g.authMiddleware)
	r.Use(g.limiter.Middleware(func(r *http.Request) string {
		if res, ok := r.Context().Value(ctxResolved).(auth.Resolved); ok {
			return res.APIKey
		}
		return ""
	}))

	r.Post("/v1/chat/completions", g.handleOpenAI)
	r.Post("/v1/completions", g.handleOpenAI)
	r.Post("/v1/remote-chat", g.handleRemoteChat)
	r.Post("/v1/remote-chat/completions", g.handleRemoteChat)
	r.Post("/v1/messages", g.handleAnthropic)
	r.Post("/v1beta/models/{model}:generateContent", g.handleGemini)
	r.Post("/v1beta/models/{model}:streamGenerateContent", g.handleGemini)
	r.Get("/v1/models", g.handleListModels)

	// OpenAI Responses API (Codex CLI uses this with HTTP SSE or WebSocket)
	r.HandleFunc("/v1/responses", g.handleResponses)

	// Token counting (Anthropic format)
	r.Post("/v1/messages/count_tokens", g.handleCountTokens)

	// Compaction endpoints (Codex / Claude Code auto-compact) — see compact.go
	r.Post("/v1/codex/compact", g.handleCodexCompact)
	r.Post("/v1/claude-code/compact", g.handleClaudeCodeCompact)
	r.Post("/v1/compact", g.handleCompactGeneric)
	r.Post("/v1/compact/summary", g.handleCompactSummary)

	// -- Codex CLI compatibility endpoints -----------------------------------
	// Codex CLI uses these when OPENAI_BASE_URL points to our gateway.
	r.HandleFunc("/backend-api/codex/responses", g.handleResponses)
	// Codex compact: called when context limit is reached.
	// Expects OpenAI Responses API shape, returns non-streaming JSON summary.
	r.Post("/backend-api/codex/responses/compact", g.handleCodexCompact)
	r.Get("/backend-api/codex/models", g.handleCodexModels)
	r.Get("/backend-api/conversation_limit", g.handleCodexConversationLimit)

	// -- Claude Code CLI compatibility endpoints ------------------------------
	r.Get("/v1/organizations", g.handleClaudeOrganizations)
	r.Get("/v1/usage", g.handleClaudeUsage)
}

func (g *Gateway) networkMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ingressLimiter := g.currentIngressLimiter()
		egressLimiter := g.currentEgressLimiter()
		if ingressLimiter != nil && r.Body != nil {
			r.Body = &rateLimitedReadCloser{ReadCloser: r.Body, ctx: r.Context(), limiter: ingressLimiter}
		}
		if egressLimiter != nil && !isWebSocketUpgrade(r) {
			w = &shapedResponseWriter{ResponseWriter: w, ctx: r.Context(), limiter: egressLimiter}
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const (
	ctxResolved ctxKey = iota
)

const requestBodyReadChunk = 32 << 10

var requestBodyChunkPool = sync.Pool{
	New: func() any {
		return make([]byte, requestBodyReadChunk)
	},
}

func acquireRequestBodyChunk() []byte {
	return requestBodyChunkPool.Get().([]byte)[:requestBodyReadChunk]
}

func releaseRequestBodyChunk(buf []byte) {
	if cap(buf) < requestBodyReadChunk {
		return
	}
	requestBodyChunkPool.Put(buf[:requestBodyReadChunk])
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, X-Goog-Api-Key, Anthropic-Beta, Anthropic-Version, OpenAI-Beta")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) maxRequestBytes() int64 {
	if g != nil && g.cfg != nil && g.cfg.Server.MaxRequestBytes > 0 {
		return g.cfg.Server.MaxRequestBytes
	}
	return 512 << 20
}

func (g *Gateway) readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, func(), error) {
	maxBytes := g.maxRequestBytes()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	capacityHint := int64(0)
	if r.ContentLength > 0 && r.ContentLength <= maxBytes {
		capacityHint = r.ContentLength
	}
	return g.readAllGatedWithCapacity(r.Context(), r.Body, maxBytes, capacityHint)
}

func (g *Gateway) readAllGated(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, func(), error) {
	return g.readAllGatedWithCapacity(ctx, reader, maxBytes, 0)
}

func (g *Gateway) readAllGatedWithCapacity(ctx context.Context, reader io.Reader, maxBytes, capacityHint int64) ([]byte, func(), error) {
	reservation := g.bodyGate.newReservation()
	released := false
	releaseOnce := func() {
		if !released {
			reservation.release()
			released = true
		}
	}

	if maxBytes > 0 && capacityHint > maxBytes {
		capacityHint = maxBytes
	}
	if capacityHint < 0 {
		capacityHint = 0
	}
	if capacityHint > 0 {
		if err := reservation.grow(ctx, capacityHint); err != nil {
			releaseOnce()
			return nil, nil, err
		}
	}

	initialCap := requestBodyReadChunk
	if capacityHint > int64(initialCap) {
		if capacityHint > int64(int(^uint(0)>>1)) {
			initialCap = int(^uint(0) >> 1)
		} else {
			initialCap = int(capacityHint)
		}
	}
	body := make([]byte, 0, initialCap)
	chunk := acquireRequestBodyChunk()
	defer releaseRequestBodyChunk(chunk)
	var total int64
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			total += int64(n)
			if maxBytes > 0 && total > maxBytes {
				releaseOnce()
				return nil, nil, fmt.Errorf("request body exceeds %d bytes", maxBytes)
			}
			if growErr := reservation.grow(ctx, total); growErr != nil {
				releaseOnce()
				return nil, nil, growErr
			}
			body = append(body, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return body, releaseOnce, nil
			}
			releaseOnce()
			return nil, nil, err
		}
	}
}

func (g *Gateway) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/healthz" || strings.HasPrefix(path, "/metrics") {
			next.ServeHTTP(w, r)
			return
		}
		res, ok := g.resolver.Resolve(
			r.Header.Get("Authorization"),
			r.Header.Get("x-api-key"),
			r.Header.Get("x-goog-api-key"),
		)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"type":    "invalid_api_key",
					"message": "API key is missing or invalid",
				},
			})
			return
		}
		ctx := context.WithValue(r.Context(), ctxResolved, res)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (g *Gateway) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		g.logger.Info("request",
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"req_id", middleware.GetReqID(r.Context()),
		)
	})
}

func (g *Gateway) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	body = stealth.ScrubRequestBody(body)
	req, err := openai.DecodeBytes(body)
	if err != nil {
		metrics.TranslationErrors.WithLabelValues("openai", "ir").Inc()
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil
	g.serveRequest(w, r, res, req, "openai")
}

func (g *Gateway) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	body = stealth.ScrubRequestBody(body)
	req, err := anthropic.Decode(bytes.NewReader(body))
	if err != nil {
		metrics.TranslationErrors.WithLabelValues("anthropic", "ir").Inc()
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil
	g.serveRequest(w, r, res, req, "anthropic")
}

func (g *Gateway) handleGemini(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	model := chi.URLParam(r, "model")
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	req, err := geminiproto.Decode(bytes.NewReader(body), model)
	if err != nil {
		metrics.TranslationErrors.WithLabelValues("gemini", "ir").Inc()
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
		req.Stream = true
	}
	body = nil
	g.serveRequest(w, r, res, req, "gemini")
}

// serveRequest is the central pipeline: model rewrite → schedule → invoke →
// head-buffer failover → encode response in inboundProto.
func (g *Gateway) serveRequest(w http.ResponseWriter, r *http.Request, res auth.Resolved, req *ir.Request, inboundProto string) {
	start := time.Now()

	effective, err := router.RewriteModel(req, res)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("model_not_allowed", err.Error()))
		return
	}
	originalModel := req.OriginalModel
	if originalModel == "" {
		originalModel = req.Model
	}
	req.Model = effective

	// Normalize request for deterministic bytes → maximizes upstream prefix-cache hits.
	normalize.Request(req)

	// Inject group-level system prompt (used by security / specialised groups).
	applyGroupSystemPrompt(req, res.Group)

	// Non-Claude providers don't natively understand Claude Code's plan mode,
	// multi-agent, or interactive tools. Inject a brief behavioral hint so
	// GPT/Gemini learns to call EnterPlanMode, Agent, etc. instead of dumping
	// a wall of text. Only injected when these tools are present in the request.
	// Must run BEFORE cacheopt.Apply so the system prompt is complete when
	// cache breakpoint decisions are made.
	if res.Group != nil && res.Group.Provider != "claude" {
		injectToolBehaviorHints(req)
	}

	// Group-level overrides and cache optimisations.
	if res.Group != nil {
		// ForcedModel: admin pins a specific model for this group.
		if res.Group.ForcedModel != "" {
			req.Model = res.Group.ForcedModel
			effective = res.Group.ForcedModel
		}
		// Cache optimisations (tool schema normalisation + cache_control injection).
		cacheopt.Apply(req, res.Group.Provider)
		// Reasoning effort override.
		if res.Group.ReasoningEffort != "" {
			req.ReasoningEffort = res.Group.ReasoningEffort
			if res.Group.Provider == "claude" {
				req.ThinkingTokens = effortToClaudeBudget(res.Group.ReasoningEffort)
			}
		}
	}

	// Cyber groups: add lab-context framing to shift GPT/Gemini's content
	// classifier perception for security-sensitive queries.
	if res.Group != nil {
		enhanceCyberContext(req, res.Group.ID)
	}

	convHash := router.HashConversation(req)
	req.UpstreamSessionKey = buildUpstreamSessionKey(r, res, req, convHash)

	prevConv, hadPrev := g.convMap.Lookup(convHash)
	_ = prevConv
	_ = hadPrev

	if res.Group == nil && res.Federation == nil {
		writeJSON(w, http.StatusInternalServerError, errResp("no_group", "resolved group or federation missing"))
		return
	}

	var staticProv provider.Provider
	if res.Group != nil {
		var ok bool
		staticProv, ok = g.providers.Get(res.Group.Provider)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errResp("unknown_provider", res.Group.Provider))
			return
		}
	}

	// ── Stealth: tool name obfuscation ──
	var toolRewriter *stealth.ToolRewriter
	if !preserveAnthropicClaudeCodeShape(req, res.Group) {
		toolRewriter = stealth.NewToolRewriter(req.Tools)
	}
	if toolRewriter != nil {
		toolRewriter.Apply(req)
	}

	// ── Stealth: scrub proxy-revealing headers from inbound request ──
	stealth.ScrubProxy(r.Header)

	// ── Stealth: request jitter (human-like Pareto distribution) ──
	if jitter := stealth.RequestJitter(g.cfg.Stealth.MaxRPSPerPersona, g.cfg.Stealth.JitterPercent); jitter > 0 {
		time.Sleep(jitter)
	}

	var pickReq scheduler.PickRequest
	if res.Federation != nil {
		var allAccountIDs []string
		provSet := map[string]bool{}
		for _, gid := range res.Federation.MemberGroups {
			if mg, ok := g.resolver.GroupByID(gid); ok {
				allAccountIDs = append(allAccountIDs, mg.AccountIDs...)
				provSet[mg.Provider] = true
			}
		}
		var providers []string
		for p := range provSet {
			providers = append(providers, p)
		}
		pickReq = scheduler.PickRequest{
			TenantID:   res.Federation.TenantID,
			Providers:  providers,
			Model:      effective,
			ConvHash:   convHash,
			SystemHash: normalize.HashSystemPrefix(req),
			AccountIDs: allAccountIDs,
		}
	} else {
		pickReq = scheduler.PickRequest{
			GroupID:    res.Group.ID,
			TenantID:   res.Group.TenantID,
			Provider:   res.Group.Provider,
			Model:      effective,
			ConvHash:   convHash,
			SystemHash: normalize.HashSystemPrefix(req),
			AccountIDs: res.Group.AccountIDs,
		}
	}
	kiroFallback := false
	if res.Federation == nil {
		pickReq, kiroFallback = g.claudeKiroFallbackPick(pickReq, res)
	}

	encoder := newEncoderForProto(inboundProto, w, originalModel, req.Stream)
	encoder.WriteHeaders()

	var usageInput, usageOutput, cacheReadTok, cacheCreationTok int
	commit := func(ev ir.Event) error {
		// Restore obfuscated tool names in response events.
		if toolRewriter != nil && ev.Kind == ir.EvToolUseStart {
			ev.ToolName = string(toolRewriter.Restore([]byte(ev.ToolName)))
		}
		if ev.Kind == ir.EvUsage {
			if ev.InputTokens > 0 {
				usageInput = ev.InputTokens
			}
			if ev.OutputTokens > 0 {
				usageOutput = ev.OutputTokens
			}
			if ev.CacheReadTokens > 0 {
				cacheReadTok = ev.CacheReadTokens
			}
			if ev.CacheCreationTokens > 0 {
				cacheCreationTok = ev.CacheCreationTokens
			}
		}
		return encoder.WriteEvent(ev)
	}

	failoverCtx, cancel := context.WithTimeout(r.Context(), g.cfg.Scheduler.Failover.NeverFail.MaxWait+g.cfg.Server.WriteTimeout)
	defer cancel()

	mux := &stream.MultiplexInvoker{
		HeadBuffer: stream.HeadBufferConfig{
			MaxDuration: g.cfg.Scheduler.Failover.HeadBuffer.MaxDuration,
			MaxBytes:    g.cfg.Scheduler.Failover.HeadBuffer.MaxBytes,
			MaxEvents:   g.cfg.Scheduler.Failover.HeadBuffer.MaxEvents,
		},
		MaxAttempts: g.cfg.Scheduler.Retry.MaxAttempts,
	}

	var lastAccountID string
	var lastProvider string

	err = mux.Run(failoverCtx,
		func(attempt int) (<-chan ir.Event, func(), error) {
			slot, perr := g.sched.Pick(failoverCtx, pickReq)
			if perr != nil {
				return nil, func() {}, perr
			}
			lastAccountID = slot.Account.ID
			lastProvider = slot.Account.Provider
			g.sched.IncInflight(slot.Account.ID)
			prov := staticProv
			if res.Federation != nil || (res.Group != nil && slot.Account.Provider != res.Group.Provider) {
				var ok bool
				prov, ok = g.providers.Get(slot.Account.Provider)
				if !ok {
					g.sched.DecInflight(slot.Account.ID)
					return nil, func() {}, fmt.Errorf("no provider for %s", slot.Account.Provider)
				}
			}
			invokeReq := req
			if g.identity != nil {
				rewriter, rerr := g.identity.RewriterForAccount(slot.Account.ID, slot.Account.Provider, slot.Account.Email)
				if rerr != nil {
					g.sched.DecInflight(slot.Account.ID)
					return nil, func() {}, fmt.Errorf("identity rewrite: %w", rerr)
				}
				invokeReq = cloneIRRequest(req)
				rewriter.RewriteIRRequest(invokeReq)
			}
			ch, ierr := prov.Invoke(failoverCtx, slot.Account, invokeReq)
			if ierr != nil {
				g.sched.DecInflight(slot.Account.ID)
				class := scheduler.ClassifyError(0, "", ierr)
				g.sched.MarkFailure(slot.Account.ID, class)
				if attempt > 0 {
					metrics.FailoverSilent.WithLabelValues("preflight").Inc()
				}
				return nil, func() {}, ierr
			}
			acctID := slot.Account.ID
			release := func() {
				g.sched.DecInflight(acctID)
			}
			if attempt > 0 {
				metrics.FailoverSilent.WithLabelValues("head_buffer").Inc()
			}
			return ch, release, nil
		},
		commit,
	)

	// Mark account success or failure based on mux.Run result.
	if err != nil {
		if lastAccountID != "" {
			class := scheduler.ClassifyError(0, "", err)
			g.sched.MarkFailure(lastAccountID, class)
		}
		// Phase C best-effort close: emit Done so downstream SDKs don't surface
		// connection errors.
		if errors.Is(err, context.Canceled) {
			return
		}
		_ = encoder.WriteEvent(ir.Event{Kind: ir.EvDone, FinishReason: "error"})
		metrics.FailoverVisible.WithLabelValues("midstream").Inc()
		if lastAccountID != "" {
			g.logger.Warn("midstream_failure", "account", lastAccountID, "err", err.Error())
		}
	} else if lastAccountID != "" {
		dur := time.Since(start).Seconds()
		g.sched.MarkSuccess(lastAccountID, dur*1000)
	}

	encoder.WriteFinal()
	dur := time.Since(start).Seconds()

	var mGroupID, mTenantID, mProvider string
	if res.Group != nil {
		mGroupID = res.Group.ID
		mTenantID = res.Group.TenantID
		mProvider = res.Group.Provider
	} else if res.Federation != nil {
		mGroupID = res.Federation.ID
		mTenantID = res.Federation.TenantID
		mProvider = "federation"
	}
	if lastProvider != "" && (res.Federation != nil || kiroFallback) {
		mProvider = lastProvider
	}

	metrics.RequestDuration.WithLabelValues(mGroupID, mProvider, originalModel).Observe(dur)
	status := "ok"
	if err != nil {
		status = "error"
	}
	metrics.RequestTotal.WithLabelValues(mGroupID, mProvider, originalModel, status).Inc()

	// Sticky routing record (for account affinity, separate from cache hit).
	if status == "ok" && lastAccountID != "" && convHash != "" {
		stickyHit := g.convMap.Record(convHash, lastAccountID, res.APIKey, mTenantID, mGroupID, len(req.Messages))
		if stickyHit {
			metrics.CacheStickyHit.WithLabelValues(mGroupID).Inc()
		} else {
			metrics.CacheReplay.WithLabelValues(mGroupID).Inc()
		}
	}

	// Cache hit = upstream actually returned cache_read tokens > 0.
	// This is the real signal from Anthropic/OpenAI that prompt caching worked.
	cacheHit := cacheReadTok > 0

	if g.store != nil {
		_ = g.store.AppendRequestSample(context.Background(), store.RequestSample{
			At:                  time.Now(),
			TenantID:            mTenantID,
			GroupID:             mGroupID,
			Provider:            mProvider,
			Model:               originalModel,
			AccountID:           lastAccountID,
			APIKey:              res.APIKey,
			CacheHit:            cacheHit,
			LatencyMs:           int64(dur * 1000),
			InputTokens:         usageInput,
			OutputTokens:        usageOutput,
			CacheReadTokens:     cacheReadTok,
			CacheCreationTokens: cacheCreationTok,
			Status:              status,
		})
	}
	if g.audit != nil && err != nil {
		g.audit.Log("warn", "request", lastAccountID, mGroupID, "request failed: "+err.Error())
	}
	// Async quota refresh: push provider-cached rate limit data to scheduler
	// so the dashboard updates in real-time without waiting for discovery runner.
	if status == "ok" && lastAccountID != "" && g.QuotaRefreshFunc != nil {
		go g.QuotaRefreshFunc(lastAccountID)
	}
}

func (g *Gateway) claudeKiroFallbackPick(primary scheduler.PickRequest, res auth.Resolved) (scheduler.PickRequest, bool) {
	if res.Group == nil || res.Group.Provider != "claude" || g.sched == nil || g.providers == nil {
		return primary, false
	}
	if g.sched.HasUsable(primary) {
		return primary, false
	}
	if _, ok := g.providers.Get("kiro"); !ok {
		return primary, false
	}
	fallback := primary
	fallback.Provider = "kiro"
	fallback.Providers = nil
	fallback.AccountIDs = nil
	fallback.PreferredAccountID = ""
	fallback.ExcludeAccountIDs = nil
	if !g.sched.HasUsable(fallback) {
		return primary, false
	}
	return fallback, true
}

// EventWriter abstracts the per-protocol encoder so the gateway can stream
// IR events into whatever wire format the client expects.
type EventWriter interface {
	WriteHeaders()
	WriteEvent(ir.Event) error
	WriteFinal()
}

func newEncoderForProto(proto string, w http.ResponseWriter, displayModel string, streamReq bool) EventWriter {
	switch proto {
	case "anthropic":
		return &anthropicWriter{enc: anthropic.NewEncoder(w, displayModel, streamReq), stream: streamReq}
	case "gemini":
		return &geminiWriter{enc: geminiproto.NewEncoder(w, displayModel, streamReq), stream: streamReq}
	default:
		return &openaiWriter{enc: openai.NewEncoder(w, displayModel, streamReq), stream: streamReq}
	}
}

func preserveAnthropicClaudeCodeShape(req *ir.Request, group *domain.Group) bool {
	return req != nil &&
		group != nil &&
		group.Provider == "claude" &&
		req.OriginalProto == "anthropic" &&
		(len(req.AnthropicSystem) > 0 ||
			len(req.AnthropicMetadata) > 0 ||
			len(req.AnthropicContextManagement) > 0)
}

func buildUpstreamSessionKey(r *http.Request, res auth.Resolved, req *ir.Request, convHash string) string {
	if req == nil {
		return ""
	}
	groupID := ""
	if res.Group != nil {
		groupID = res.Group.ID
	} else if res.Federation != nil {
		groupID = "federation:" + res.Federation.ID
	}
	tenantID := ""
	if res.Tenant != nil {
		tenantID = res.Tenant.ID
	}
	anchor := firstUserText(req)
	if anchor == "" {
		anchor = convHash
	}
	if anchor == "" {
		anchor = req.System
	}
	if anchor == "" && groupID == "" && res.APIKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"session-v1",
		tenantID,
		groupID,
		res.APIKey,
		clientIPForSession(r),
		req.OriginalProto,
		req.OriginalModel,
		anchor,
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func firstUserText(req *ir.Request) string {
	if req == nil {
		return ""
	}
	for _, msg := range req.Messages {
		if msg.Role != ir.RoleUser {
			continue
		}
		for _, part := range msg.Parts {
			if part.Kind == ir.PartText && strings.TrimSpace(part.Text) != "" {
				return part.Text
			}
		}
	}
	return ""
}

func clientIPForSession(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, key := range []string{"X-Real-IP", "Cf-Connecting-IP", "True-Client-IP"} {
		if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
			return v
		}
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		return host[:i]
	}
	return host
}

func cloneIRRequest(req *ir.Request) *ir.Request {
	if req == nil {
		return nil
	}
	cp := *req
	if req.Messages != nil {
		cp.Messages = make([]ir.Message, len(req.Messages))
		for i := range req.Messages {
			cp.Messages[i].Role = req.Messages[i].Role
			if req.Messages[i].Parts != nil {
				cp.Messages[i].Parts = make([]ir.Part, len(req.Messages[i].Parts))
				for j := range req.Messages[i].Parts {
					cp.Messages[i].Parts[j] = cloneIRPart(req.Messages[i].Parts[j])
				}
			}
		}
	}
	if req.Tools != nil {
		cp.Tools = make([]ir.ToolDef, len(req.Tools))
		for i := range req.Tools {
			cp.Tools[i] = req.Tools[i]
			cp.Tools[i].Schema = cloneBytes(req.Tools[i].Schema)
			cp.Tools[i].CacheControl = cloneBytes(req.Tools[i].CacheControl)
		}
	}
	cp.AnthropicSystem = cloneBytes(req.AnthropicSystem)
	cp.AnthropicMetadata = cloneBytes(req.AnthropicMetadata)
	cp.AnthropicContextManagement = cloneBytes(req.AnthropicContextManagement)
	cp.AnthropicToolChoice = cloneBytes(req.AnthropicToolChoice)
	return &cp
}

func cloneIRPart(p ir.Part) ir.Part {
	p.ImageBytes = cloneBytes(p.ImageBytes)
	p.ToolUseInput = cloneBytes(p.ToolUseInput)
	p.ToolResultBytes = cloneBytes(p.ToolResultBytes)
	p.CacheControl = cloneBytes(p.CacheControl)
	return p
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// Each protocol's encoder exposes Stream(<-chan Event); to match the EventWriter
// interface we adapt with a channel and a goroutine. This is fine because
// throughput is bounded by the upstream rate, not by our copy step.

type openaiWriter struct {
	enc      *openai.Encoder
	stream   bool
	ch       chan ir.Event
	doneCh   chan error
	headerOK bool
}

func (a *openaiWriter) WriteHeaders() {
	a.enc.WriteHeaders()
	a.ch = make(chan ir.Event, 32)
	a.doneCh = make(chan error, 1)
	go func() { a.doneCh <- a.enc.Stream(a.ch) }()
	a.headerOK = true
}
func (a *openaiWriter) WriteEvent(ev ir.Event) error {
	if !a.headerOK {
		return errors.New("headers not written")
	}
	select {
	case a.ch <- ev:
		return nil
	case <-time.After(60 * time.Second):
		return errors.New("encoder backpressure timeout")
	}
}
func (a *openaiWriter) WriteFinal() {
	if a.ch != nil {
		close(a.ch)
		select {
		case <-a.doneCh:
		case <-time.After(5 * time.Second):
		}
	}
}

type anthropicWriter struct {
	enc      *anthropic.Encoder
	stream   bool
	ch       chan ir.Event
	doneCh   chan error
	headerOK bool
}

func (a *anthropicWriter) WriteHeaders() {
	a.enc.WriteHeaders()
	a.ch = make(chan ir.Event, 32)
	a.doneCh = make(chan error, 1)
	go func() { a.doneCh <- a.enc.Stream(a.ch) }()
	a.headerOK = true
}
func (a *anthropicWriter) WriteEvent(ev ir.Event) error {
	if !a.headerOK {
		return errors.New("headers not written")
	}
	select {
	case a.ch <- ev:
		return nil
	case <-time.After(60 * time.Second):
		return errors.New("encoder backpressure timeout")
	}
}
func (a *anthropicWriter) WriteFinal() {
	if a.ch != nil {
		close(a.ch)
		select {
		case <-a.doneCh:
		case <-time.After(5 * time.Second):
		}
	}
}

type geminiWriter struct {
	enc      *geminiproto.Encoder
	stream   bool
	ch       chan ir.Event
	doneCh   chan error
	headerOK bool
}

func (a *geminiWriter) WriteHeaders() {
	a.enc.WriteHeaders()
	a.ch = make(chan ir.Event, 32)
	a.doneCh = make(chan error, 1)
	go func() { a.doneCh <- a.enc.Stream(a.ch) }()
	a.headerOK = true
}
func (a *geminiWriter) WriteEvent(ev ir.Event) error {
	if !a.headerOK {
		return errors.New("headers not written")
	}
	select {
	case a.ch <- ev:
		return nil
	case <-time.After(60 * time.Second):
		return errors.New("encoder backpressure timeout")
	}
}
func (a *geminiWriter) WriteFinal() {
	if a.ch != nil {
		close(a.ch)
		select {
		case <-a.doneCh:
		case <-time.After(5 * time.Second):
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errResp(typ, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"type": typ, "message": msg}}
}

// SystemHandler exposes /healthz and /metrics. Mounted on the same listener.
func (g *Gateway) SystemHandler() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") {
			_, _ = w.Write([]byte("ok"))
			return
		}
		snap := g.sched.SnapshotCached(2 * time.Second)
		accounts := len(snap)
		healthy := 0
		for _, s := range snap {
			if s.Healthy {
				healthy++
			}
		}
		convStats := g.convMap.Stats()
		type healthJSON struct {
			Status          string `json:"status"`
			Accounts        int    `json:"accounts"`
			HealthyAccounts int    `json:"healthy_accounts"`
			CacheEntries    int    `json:"cache_entries"`
			CacheHits       int64  `json:"cache_hits"`
			CacheMisses     int64  `json:"cache_misses"`
		}
		status := "ok"
		if healthy == 0 && accounts > 0 {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(healthJSON{
			Status:          status,
			Accounts:        accounts,
			HealthyAccounts: healthy,
			CacheEntries:    convStats.Entries,
			CacheHits:       convStats.Hits,
			CacheMisses:     convStats.Misses,
		})
	})
	r.Handle("/metrics", promhttp.Handler())
	return r
}

// SnapshotJSON returns a JSON snapshot of scheduler + accounts for cluster API.
func (g *Gateway) SnapshotJSON() ([]byte, error) {
	type snap struct {
		Identity  string               `json:"identity"`
		Region    string               `json:"region"`
		Tenants   []*domain.Tenant     `json:"tenants"`
		Accounts  []scheduler.SlotView `json:"accounts"`
		Generated time.Time            `json:"generated"`
	}
	s := snap{
		Identity:  g.cfg.Cluster.Identity,
		Region:    g.cfg.Cluster.Region,
		Accounts:  g.sched.SnapshotCached(2 * time.Second),
		Generated: time.Now(),
	}
	return json.Marshal(s)
}

// helpful for tests
func (g *Gateway) Logger() *slog.Logger { return g.logger }

func (g *Gateway) String() string {
	return fmt.Sprintf("Gateway(provider_count=%d)", 0)
}

// injectToolBehaviorHints appends a brief instruction to the system prompt so
// non-Claude models (GPT, Gemini) correctly use Claude Code's interactive tools:
//   - EnterPlanMode / ExitPlanMode — interactive planning (plan mode)
//   - Agent — parallel sub-agent spawning (multi-agent)
//   - TaskCreate / TaskUpdate — structured task tracking
//
// Without this hint, GPT/Gemini tend to output a wall of text instead of calling
// these tools, breaking plan mode's interactive Q&A and multi-agent concurrency.
func injectToolBehaviorHints(req *ir.Request) {
	var hasPlan, hasAgent bool
	for _, t := range req.Tools {
		switch t.Name {
		case "EnterPlanMode":
			hasPlan = true
		case "Agent":
			hasAgent = true
		}
	}
	if !hasPlan && !hasAgent {
		return
	}

	var hint strings.Builder
	hint.WriteString("\n\n[Tool Usage Rules]\n")

	if hasPlan {
		hint.WriteString(
			"When the user asks you to plan, discuss strategy, or when you need to clarify requirements before coding:\n" +
				"1. Call the EnterPlanMode tool FIRST to enter planning mode.\n" +
				"2. Then present your analysis as a detailed, thorough numbered plan — include file paths, code changes, and implementation steps. Do NOT abbreviate or summarize.\n" +
				"3. Ask ONE focused question at a time and wait for the user's response before continuing.\n" +
				"4. When the plan is confirmed, call ExitPlanMode to begin implementation.\n" +
				"NEVER dump an entire plan as a single long text block — always use the plan mode tools for interactive discussion.\n")
	}
	if hasAgent {
		hint.WriteString(
			"When a task can be parallelized (e.g. research + implementation, or modifying independent files):\n" +
				"1. Call MULTIPLE Agent tools in the SAME response to run sub-agents concurrently.\n" +
				"2. Each Agent call should have a clear, self-contained prompt describing its sub-task.\n" +
				"3. Prefer parallel agents over sequential steps when the sub-tasks are independent.\n")
	}

	req.System += hint.String()
}

// effortToClaudeBudget maps a named effort level to Claude's thinking budget_tokens.
// Claude uses budget_tokens (integer) rather than a named effort string.
// Mapping mirrors Anthropic's own effort presets:
//
//	none → 0 (thinking disabled)
//	low  → 1024
//	medium → 8000
//	high → 16000
//	max  → 32000
func effortToClaudeBudget(effort string) int {
	switch effort {
	case "none", "0":
		return 0
	case "low", "1", "minimal":
		return 1024
	case "medium", "2", "normal":
		return 8000
	case "high", "3":
		return 16000
	case "max", "maximum", "4":
		return 32000
	}
	// Try to parse as raw integer token count.
	var _n int
	if _, _err := fmt.Sscanf(effort, "%d", &_n); _err == nil && _n > 0 {
		return _n
	}
	return 8000 // default medium
}
