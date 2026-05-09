package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/metrics"
	"github.com/llm-pool/gateway/internal/normalize"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/protocol/openai"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/stealth"
	"github.com/llm-pool/gateway/internal/store"
)

var (
	errRemoteChatNotConfigured = errors.New("remote chat account is not configured")
	errRemoteChatAccountGone   = errors.New("configured remote chat account was not found")
)

type simpleRemoteChatRequest struct {
	Message         *string         `json:"message,omitempty"`
	Messages        json.RawMessage `json:"messages,omitempty"`
	Model           string          `json:"model,omitempty"`
	System          string          `json:"system,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type remoteChatStats struct {
	inputTokens         int
	outputTokens        int
	cacheReadTokens     int
	cacheCreationTokens int
}

func (s *remoteChatStats) observe(ev ir.Event) {
	if ev.Kind != ir.EvUsage {
		return
	}
	if ev.InputTokens > 0 {
		s.inputTokens = ev.InputTokens
	}
	if ev.OutputTokens > 0 {
		s.outputTokens = ev.OutputTokens
	}
	if ev.CacheReadTokens > 0 {
		s.cacheReadTokens = ev.CacheReadTokens
	}
	if ev.CacheCreationTokens > 0 {
		s.cacheCreationTokens = ev.CacheCreationTokens
	}
}

func (g *Gateway) handleRemoteChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)

	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	body = stealth.ScrubRequestBody(body)

	req, err := decodeRemoteChatRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil

	accountID, acc, err := g.remoteChatConfiguredAccount(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, errRemoteChatNotConfigured):
			writeJSON(w, http.StatusConflict, errResp("remote_chat_not_configured", err.Error()))
		case errors.Is(err, errRemoteChatAccountGone):
			writeJSON(w, http.StatusConflict, errResp("remote_chat_account_missing", err.Error()))
		default:
			writeJSON(w, http.StatusInternalServerError, errResp("remote_chat_config_error", err.Error()))
		}
		return
	}
	if acc.State != "" && acc.State != domain.StateActive {
		writeJSON(w, http.StatusConflict, errResp("remote_chat_account_inactive", fmt.Sprintf("configured account %s is %s", accountID, acc.State)))
		return
	}
	if req.Model == "" {
		req.Model = remoteChatDefaultModel(acc)
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, errResp("model_required", "model is required when the configured account has no discovered default model"))
		return
	}
	if req.OriginalModel == "" {
		req.OriginalModel = req.Model
	}
	if req.OriginalProto == "" {
		req.OriginalProto = "remote_chat"
	}
	normalize.Request(req)

	if g.providers == nil {
		writeJSON(w, http.StatusInternalServerError, errResp("unknown_provider", "provider registry is not configured"))
		return
	}
	prov, ok := g.providers.Get(acc.Provider)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errResp("unknown_provider", acc.Provider))
		return
	}

	if g.sched != nil {
		g.sched.IncInflight(accountID)
		defer g.sched.DecInflight(accountID)
	}
	ch, err := prov.Invoke(r.Context(), acc, req)
	if err != nil {
		g.finishRemoteChatRequest(start, res, acc, req, remoteChatStats{}, "error", err)
		writeJSON(w, http.StatusBadGateway, errResp("upstream_error", err.Error()))
		return
	}

	displayModel := req.OriginalModel
	if displayModel == "" {
		displayModel = req.Model
	}
	var stats remoteChatStats
	responseStarted := false
	if req.Stream {
		responseStarted = true
		err = writeRemoteChatStream(r.Context(), w, displayModel, ch, &stats)
	} else {
		var events []ir.Event
		events, err = collectRemoteChatEvents(r.Context(), ch, &stats)
		if err == nil {
			enc := openai.NewEncoder(w, displayModel, false)
			enc.WriteHeaders()
			responseStarted = true
			err = enc.Stream(eventsFromSlice(events))
		}
	}

	status := "ok"
	if err != nil {
		status = "error"
	}
	g.finishRemoteChatRequest(start, res, acc, req, stats, status, err)
	if err != nil && !responseStarted {
		writeJSON(w, http.StatusBadGateway, errResp("upstream_error", err.Error()))
	}
}

func decodeRemoteChatRequest(body []byte) (*ir.Request, error) {
	var simple simpleRemoteChatRequest
	if err := json.Unmarshal(body, &simple); err == nil && simple.Message != nil && len(simple.Messages) == 0 {
		if strings.TrimSpace(*simple.Message) == "" {
			return nil, errors.New("message required")
		}
		return &ir.Request{
			Model:           strings.TrimSpace(simple.Model),
			System:          simple.System,
			Messages:        []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: *simple.Message}}}},
			Temperature:     simple.Temperature,
			TopP:            simple.TopP,
			MaxTokens:       simple.MaxTokens,
			Stream:          simple.Stream,
			ReasoningEffort: simple.ReasoningEffort,
			OriginalModel:   strings.TrimSpace(simple.Model),
			OriginalProto:   "remote_chat",
		}, nil
	}

	req, err := openai.DecodeBytes(body)
	if err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 && strings.TrimSpace(req.System) == "" {
		return nil, errors.New("message or messages required")
	}
	return req, nil
}

func (g *Gateway) remoteChatConfiguredAccount(ctx context.Context) (string, *domain.Account, error) {
	if g.store == nil {
		return "", nil, errors.New("store is not configured")
	}
	accountID, ok, err := g.store.GetSetting(ctx, store.SettingRemoteChatAccountID)
	if err != nil {
		return "", nil, err
	}
	accountID = strings.TrimSpace(accountID)
	if !ok || accountID == "" {
		return "", nil, errRemoteChatNotConfigured
	}
	acc, err := g.store.GetAccount(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return accountID, nil, errRemoteChatAccountGone
	}
	if err != nil {
		return accountID, nil, err
	}
	if g.sched != nil {
		if live, ok := g.sched.AccountByID(accountID); ok {
			return accountID, live, nil
		}
	}
	return accountID, acc, nil
}

func remoteChatDefaultModel(acc *domain.Account) string {
	for _, m := range acc.Quota.DiscoveredModels {
		if m.ID != "" && m.Available {
			return m.ID
		}
	}
	for _, m := range acc.Quota.DiscoveredModels {
		if m.ID != "" {
			return m.ID
		}
	}
	switch acc.Provider {
	case "chatgpt", "blink":
		return "gpt-5.2"
	case "claude", "kiro", "windsurf", "trae":
		return "claude-sonnet-4-6"
	case "gemini":
		return "gemini-2.5-pro"
	case "grok":
		return "grok-4"
	case "cursor", "openblocklabs":
		return "gpt-4o"
	case "cerebras":
		return "llama-3.3-70b"
	default:
		return ""
	}
}

func collectRemoteChatEvents(ctx context.Context, events <-chan ir.Event, stats *remoteChatStats) ([]ir.Event, error) {
	out := make([]ir.Event, 0, 16)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return out, nil
			}
			stats.observe(ev)
			if ev.Kind == ir.EvError {
				if ev.Err != nil {
					return nil, ev.Err
				}
				return nil, errors.New("upstream error")
			}
			out = append(out, ev)
		}
	}
}

func eventsFromSlice(events []ir.Event) <-chan ir.Event {
	ch := make(chan ir.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

func writeRemoteChatStream(ctx context.Context, w http.ResponseWriter, displayModel string, events <-chan ir.Event, stats *remoteChatStats) error {
	writer := newEncoderForProto("openai", w, displayModel, true)
	writer.WriteHeaders()
	for {
		select {
		case <-ctx.Done():
			writer.WriteFinal()
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				writer.WriteFinal()
				return nil
			}
			stats.observe(ev)
			if ev.Kind == ir.EvError {
				_ = writer.WriteEvent(ir.Event{Kind: ir.EvDone, FinishReason: "error"})
				writer.WriteFinal()
				if ev.Err != nil {
					return ev.Err
				}
				return errors.New("upstream error")
			}
			if err := writer.WriteEvent(ev); err != nil {
				writer.WriteFinal()
				return err
			}
		}
	}
}

func (g *Gateway) finishRemoteChatRequest(start time.Time, res auth.Resolved, acc *domain.Account, req *ir.Request, stats remoteChatStats, status string, err error) {
	dur := time.Since(start)
	if g.sched != nil {
		if status == "ok" {
			g.sched.MarkSuccess(acc.ID, float64(dur.Milliseconds()))
		} else {
			g.sched.MarkFailure(acc.ID, scheduler.ClassifyError(0, "", err))
		}
	}

	tenantID := ""
	if res.Tenant != nil {
		tenantID = res.Tenant.ID
	}
	if tenantID == "" && res.Group != nil {
		tenantID = res.Group.TenantID
	}
	if tenantID == "" && res.Federation != nil {
		tenantID = res.Federation.TenantID
	}

	model := req.OriginalModel
	if model == "" {
		model = req.Model
	}
	const groupID = "remote_chat"
	metrics.RequestDuration.WithLabelValues(groupID, acc.Provider, model).Observe(dur.Seconds())
	metrics.RequestTotal.WithLabelValues(groupID, acc.Provider, model, status).Inc()
	if g.store != nil {
		_ = g.store.AppendRequestSample(context.Background(), store.RequestSample{
			At:                  time.Now(),
			TenantID:            tenantID,
			GroupID:             groupID,
			Provider:            acc.Provider,
			Model:               model,
			AccountID:           acc.ID,
			APIKey:              res.APIKey,
			CacheHit:            stats.cacheReadTokens > 0,
			LatencyMs:           dur.Milliseconds(),
			InputTokens:         stats.inputTokens,
			OutputTokens:        stats.outputTokens,
			CacheReadTokens:     stats.cacheReadTokens,
			CacheCreationTokens: stats.cacheCreationTokens,
			Status:              status,
		})
	}
	if g.audit != nil && err != nil {
		g.audit.Log("warn", "remote_chat", acc.ID, groupID, "request failed: "+err.Error())
	}
	if status == "ok" && g.QuotaRefreshFunc != nil {
		go g.QuotaRefreshFunc(acc.ID)
	}
}
