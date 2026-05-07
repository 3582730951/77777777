// Package claude is a provider adapter for Anthropic Claude (claude.ai subscription).
// Real mode calls api.anthropic.com/v1/messages with OAuth bearer tokens,
// mimicking the official Claude Code CLI headers so the OAuth credentials are
// not billed as third-party usage.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/billing"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const (
	ModeMock = "mock"
	ModeReal = "real"

	// betaHeader is the COMPLETE list sent by Claude Code CLI 2.1.92 (2026-04
	// traffic capture). ALL betas must be present — Anthropic uses this set to
	// decide whether to charge subscription quota vs. third-party extra usage.
	// Missing any beta → downgrade to extra-usage → `Third-party apps now draw
	// from your extra usage, not your plan limits.`
	betaHeader = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14," +
		"fine-grained-tool-streaming-2025-05-14,prompt-caching-scope-2026-01-05," +
		"effort-2025-11-24,redact-thinking-2026-02-12,context-management-2025-06-27," +
		"extended-cache-ttl-2025-04-11"

	// cliVersion and cliUserAgent mimic Claude Code CLI 2.1.92.
	cliVersion   = "2.1.92"
	cliUserAgent = "claude-cli/" + cliVersion + " (external, cli)"

	claudeAPIBase = "https://api.anthropic.com"
)

type Provider struct {
	Mode       string
	store      *store.Store
	resolver   *sessionResolver
	httpClient *http.Client
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

// RefreshCredential resolves the stored OAuth blob and refreshes it when due
// without calling quota or model endpoints.
func (p *Provider) RefreshCredential(ctx context.Context, acc *domain.Account) error {
	if p.Mode == ModeMock {
		return nil
	}
	if p.store == nil {
		return errors.New("store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("get secret: %w", err)
	}
	_, err = p.resolveSession(ctx, acc, sec)
	return err
}

func New(mode string) *Provider {
	if mode == "" {
		mode = ModeMock
	}
	return &Provider{
		Mode:       mode,
		resolver:   newSessionResolver(),
		httpClient: transport.Claude,
	}
}

func (p *Provider) Name() string { return "claude" }

func (p *Provider) resolveSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret) (sessionInfo, error) {
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, sec.RefreshToken)
	if err != nil {
		return sessionInfo{}, err
	}
	if err := p.persistResolvedSession(ctx, acc, sec, info); err != nil {
		log.Printf("[claude-session] account=%s persist refreshed session: %v", acc.ID, err)
	}
	return info, nil
}

func (p *Provider) persistResolvedSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret, info sessionInfo) error {
	if p.store == nil || info.AccessToken == "" {
		return nil
	}
	changed := false
	next := sec
	if shouldPersistClaudeSession(sec.SessionToken, info) {
		next.SessionToken = buildClaudeSessionJSON(info)
		changed = true
	}
	if info.RefreshToken != "" && next.RefreshToken != info.RefreshToken {
		next.RefreshToken = info.RefreshToken
		changed = true
	}
	if info.Email != "" && acc.Email == "" {
		acc.Email = info.Email
		changed = true
	}
	if !changed {
		return nil
	}
	acc.UpdatedAt = time.Now()
	return p.store.UpsertAccount(ctx, acc, next)
}

func shouldPersistClaudeSession(existing string, info sessionInfo) bool {
	old, err := parseClaudeSessionJSON([]byte(existing))
	if err != nil {
		return true
	}
	return old.AccessToken != info.AccessToken ||
		old.RefreshToken != info.RefreshToken ||
		old.AccountUUID != info.AccountUUID ||
		old.Organization != info.Organization ||
		old.Email != info.Email ||
		!old.Expires.Truncate(time.Second).Equal(info.Expires.Truncate(time.Second))
}

func buildClaudeSessionJSON(info sessionInfo) string {
	out := map[string]any{
		"access_token":      info.AccessToken,
		"refresh_token":     info.RefreshToken,
		"organization_uuid": info.Organization,
		"account_uuid":      info.AccountUUID,
		"email":             info.Email,
		"expires_at":        info.Expires.Format(time.RFC3339),
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	switch p.Mode {
	case ModeMock:
		return p.invokeMock(ctx, acc, req)
	case ModeReal:
		return p.invokeReal(ctx, acc, req)
	}
	return nil, fmt.Errorf("unknown claude mode: %s", p.Mode)
}

func (p *Provider) Probe(ctx context.Context, acc *domain.Account) error {
	if p.Mode == ModeMock {
		return nil
	}
	if p.store == nil {
		return errors.New("store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("get secret: %w", err)
	}
	_, err = p.resolveSession(ctx, acc, sec)
	return err
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	// Full model list from CPA models.json [claude] section — all tiers.
	models := []domain.ModelCapability{
		{ID: "claude-opus-4-7", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-6", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-5-20251101", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-20250514", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-1-20250805", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-sonnet-4-6", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-sonnet-4-5-20250929", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-sonnet-4-20250514", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-3-7-sonnet-20250219", Available: true, ContextWindow: 200000, SupportsTools: true, SupportsVision: true},
		{ID: "claude-haiku-4-5-20251001", Available: true, ContextWindow: 200000, SupportsTools: true},
		{ID: "claude-3-5-haiku-20241022", Available: true, ContextWindow: 200000, SupportsTools: true},
	}
	state := &domain.QuotaState{
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}

	q, ok := p.resolver.GetQuota(acc.ID)
	stale := !ok || time.Since(q.UpdatedAt) > 3*time.Minute
	log.Printf("[claude-discover] account=%s cache_ok=%v stale=%v mode=%s store_nil=%v",
		acc.ID, ok, stale, p.Mode, p.store == nil)

	// Active probe: call /api/oauth/usage (same endpoint Claude Code CLI uses).
	if stale && p.Mode == ModeReal && p.store != nil {
		if err := p.fetchOAuthUsage(ctx, acc); err != nil {
			log.Printf("[claude-discover] fetchOAuthUsage failed: %v", err)
		} else {
			q, ok = p.resolver.GetQuota(acc.ID)
			log.Printf("[claude-discover] quota cached: ok=%v 5h_util=%d 5h_reset=%v 7d_util=%d reqLimit=%d",
				ok, q.FiveHourUtilization, q.FiveHourReset, q.SevenDayUtilization, q.RequestLimit)
		}
	}

	if ok {
		// Prefer oauth/usage data (FiveHourUtilization); fall back to response headers.
		if q.FiveHourUtilization > 0 || !q.FiveHourReset.IsZero() {
			state.ShortWindow = domain.QuotaWindow{
				Limit: 100, Used: float64(q.FiveHourUtilization),
				ResetAt: q.FiveHourReset, Confidence: 1.0,
			}
		} else if q.RequestLimit > 0 {
			state.ShortWindow = domain.QuotaWindow{
				Limit: float64(q.RequestLimit), Used: float64(q.RequestLimit - q.RequestRemaining),
				ResetAt: q.ResetAt, Confidence: 0.8,
			}
		}
		if q.SevenDayUtilization > 0 || !q.SevenDayReset.IsZero() {
			state.LongWindow = domain.QuotaWindow{
				Limit: 100, Used: float64(q.SevenDayUtilization),
				ResetAt: q.SevenDayReset, Confidence: 1.0,
			}
		} else if q.TokenLimit > 0 {
			state.LongWindow = domain.QuotaWindow{
				Limit: float64(q.TokenLimit), Used: float64(q.TokenLimit - q.TokenRemaining),
				Confidence: 0.8,
			}
		}
		// Populate TierWindows from dynamic tier data (populated by fetchOAuthUsage).
		if len(q.TierMap) > 0 {
			state.TierWindows = make(map[string]domain.QuotaWindow, len(q.TierMap))
			for name, t := range q.TierMap {
				state.TierWindows[name] = domain.QuotaWindow{
					Limit: 100, Used: t.Utilization,
					ResetAt: t.ResetsAt, Confidence: 1.0,
				}
			}
		}
		// Populate ExtraUsage.
		if q.ExtraEnabled {
			state.ExtraUsage = &domain.ExtraUsage{
				IsEnabled:    true,
				Currency:     q.ExtraCurrency,
				MonthlyLimit: ptrVal(q.ExtraMonthlyLimit),
				UsedCredits:  ptrVal(q.ExtraUsedCredits),
				Utilization:  ptrVal(q.ExtraUtilization),
			}
		}
	}
	return state, nil
}

// fetchOAuthUsage calls GET /api/oauth/usage to get real 5h/7d utilization.
// This is the exact endpoint Claude Code CLI uses to display usage limits.
//
// Response format:
//
//	{"five_hour":{"utilization":37.0,"resets_at":"2026-02-08T04:59:59.000000+00:00"},
//	 "seven_day":{"utilization":26.0,"resets_at":"2026-02-12T14:59:59.771647+00:00"},
//	 "seven_day_opus":null,
//	 "extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null}}
func (p *Provider) fetchOAuthUsage(ctx context.Context, acc *domain.Account) error {
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return err
	}
	info, err := p.resolveSession(ctx, acc, sec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		claudeAPIBase+"/api/oauth/usage", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", cliUserAgent)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("oauth/usage %d", resp.StatusCode)
	}

	log.Printf("[claude-quota] account=%s status=%d raw=%s",
		acc.ID, resp.StatusCode, string(body[:min(len(body), 500)]))

	// Dynamic parsing: iterate all top-level keys, try each as {utilization, resets_at}.
	snap := QuotaSnapshot{TierMap: make(map[string]tierEntry)}
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if name == "extra_usage" {
			return true // handle separately
		}
		util := value.Get("utilization")
		if !util.Exists() {
			return true
		}
		var resetAt time.Time
		if rs := value.Get("resets_at").String(); rs != "" {
			resetAt, _ = time.Parse(time.RFC3339Nano, rs)
		}
		snap.TierMap[name] = tierEntry{Utilization: util.Float(), ResetsAt: resetAt}
		// Backward compat: populate legacy fields.
		switch name {
		case "five_hour":
			snap.FiveHourUtilization = int64(util.Float())
			snap.FiveHourReset = resetAt
		case "seven_day":
			snap.SevenDayUtilization = int64(util.Float())
			snap.SevenDayReset = resetAt
		}
		return true
	})

	// Parse extra_usage.
	if eu := gjson.GetBytes(body, "extra_usage"); eu.Exists() {
		snap.ExtraEnabled = eu.Get("is_enabled").Bool()
		if v := eu.Get("monthly_limit"); v.Exists() {
			f := v.Float()
			snap.ExtraMonthlyLimit = &f
		}
		if v := eu.Get("used_credits"); v.Exists() {
			f := v.Float()
			snap.ExtraUsedCredits = &f
		}
		if v := eu.Get("utilization"); v.Exists() {
			f := v.Float()
			snap.ExtraUtilization = &f
		}
		snap.ExtraCurrency = eu.Get("currency").String()
	}

	log.Printf("[claude-quota] account=%s tiers=%d extra_enabled=%v",
		acc.ID, len(snap.TierMap), snap.ExtraEnabled)

	p.resolver.UpdateQuota(acc.ID, snap)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ----- mock -----

func (p *Provider) invokeMock(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	out := make(chan ir.Event, 16)
	go func() {
		defer close(out)
		var lastUserText string
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == ir.RoleUser {
				for _, pp := range req.Messages[i].Parts {
					if pp.Kind == ir.PartText {
						lastUserText = pp.Text
						break
					}
				}
				break
			}
		}
		preview := strings.TrimSpace(lastUserText)
		if len(preview) > 60 {
			preview = preview[:60] + "..."
		}
		text := fmt.Sprintf("[mock claude provider · account=%s · model=%s] Echo: %q", acc.ID, req.Model, preview)
		for _, w := range strings.Fields(text) {
			select {
			case <-ctx.Done():
				out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
				return
			case out <- ir.Event{Kind: ir.EvTextDelta, Text: w + " "}:
			}
			time.Sleep(15 * time.Millisecond)
		}
		out <- ir.Event{Kind: ir.EvUsage, InputTokens: 40, OutputTokens: len(strings.Fields(text))}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "end_turn"}
	}()
	return out, nil
}

// ----- real mode (api.anthropic.com/v1/messages with OAuth) -----

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.store == nil {
		return nil, errors.New("claude: store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	info, err := p.resolveSession(ctx, acc, sec)
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	model := mapModel(req.Model)
	body, err := buildMessagesBody(req, model)
	if err != nil {
		return nil, err
	}
	// Inject / sign billing attribution block so Anthropic attributes this
	// request to Claude Code quota (not third-party extra-usage).
	body = injectAndSignBilling(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		claudeAPIBase+"/v1/messages?beta=true",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+info.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", betaHeader)
	httpReq.Header.Set("User-Agent", cliUserAgent)
	httpReq.Header.Set("X-Stainless-Lang", "js")
	httpReq.Header.Set("X-Stainless-Package-Version", "0.70.0")
	httpReq.Header.Set("X-Stainless-OS", "Linux")
	httpReq.Header.Set("X-Stainless-Arch", "arm64")
	httpReq.Header.Set("X-Stainless-Runtime", "node")
	httpReq.Header.Set("X-Stainless-Runtime-Version", "v24.13.0")
	httpReq.Header.Set("X-Stainless-Retry-Count", "0")
	httpReq.Header.Set("X-Stainless-Timeout", "600")
	httpReq.Header.Set("X-App", "cli")
	httpReq.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post messages: %w", err)
	}
	p.updateQuotaFromHeaders(acc.ID, resp.Header)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippetB(b))
	}

	out := make(chan ir.Event, 32)
	go streamClaudeSSE(ctx, resp.Body, out)
	return out, nil
}

func (p *Provider) updateQuotaFromHeaders(accountID string, h http.Header) {
	limit := parseHeaderInt(h, "anthropic-ratelimit-requests-limit")
	remaining := parseHeaderInt(h, "anthropic-ratelimit-requests-remaining")
	resetStr := h.Get("anthropic-ratelimit-requests-reset")
	tokLimit := parseHeaderInt(h, "anthropic-ratelimit-tokens-limit")
	tokRemaining := parseHeaderInt(h, "anthropic-ratelimit-tokens-remaining")
	if limit <= 0 && tokLimit <= 0 {
		return
	}
	var resetAt time.Time
	if resetStr != "" {
		resetAt, _ = time.Parse(time.RFC3339, resetStr)
	}
	p.resolver.UpdateQuota(accountID, QuotaSnapshot{
		RequestLimit:     limit,
		RequestRemaining: remaining,
		TokenLimit:       tokLimit,
		TokenRemaining:   tokRemaining,
		ResetAt:          resetAt,
	})
}

func parseHeaderInt(h http.Header, key string) int64 {
	s := h.Get(key)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// buildMessagesBody converts an IR request to the Anthropic messages API JSON.
func buildMessagesBody(req *ir.Request, model string) ([]byte, error) {
	var msgs []any
	msgIdx := 0
	for _, m := range req.Messages {
		var role string
		switch m.Role {
		case ir.RoleUser:
			role = "user"
		case ir.RoleAssistant:
			role = "assistant"
		case ir.RoleSystem:
			continue
		default:
			role = string(m.Role)
		}
		var blocks []any
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.PartText:
				if p.Text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				}
			case ir.PartImage:
				if len(p.ImageBytes) > 0 {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": p.ImageMedia,
							"data":       string(p.ImageBytes),
						},
					})
				} else if p.ImageURL != "" {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "url",
							"url":  p.ImageURL,
						},
					})
				}
			case ir.PartToolUse:
				var input any
				if len(p.ToolUseInput) > 0 {
					_ = json.Unmarshal(p.ToolUseInput, &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": p.ToolUseID,
					"name": normalizeToolName(p.ToolUseName), "input": input,
				})
			case ir.PartToolResult:
				var content any
				if len(p.ToolResultBytes) > 0 {
					_ = json.Unmarshal(p.ToolResultBytes, &content)
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_result", "tool_use_id": p.ToolResultID,
					"content": content,
				})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// Inject cache_control on the designated message's last content block
		// (typically the second-to-last user message, per CPA strategy).
		if req.MessageCacheIdx > 0 && msgIdx == req.MessageCacheIdx && len(blocks) > 0 {
			if blk, ok := blocks[len(blocks)-1].(map[string]any); ok {
				blk["cache_control"] = map[string]string{"type": "ephemeral"}
			}
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
		msgIdx++
	}

	// System prompt — wrap in array with cache_control if flagged.
	var systemField any
	if req.System != "" {
		if req.SystemCached {
			systemField = []map[string]any{{
				"type":          "text",
				"text":          req.System,
				"cache_control": map[string]any{"type": "ephemeral", "ttl": 3600},
			}}
		} else {
			systemField = req.System
		}
	}

	// Tools — normalize names to TitleCase (Claude Code convention) and inject cache_control.
	var toolsField []map[string]any
	for i, t := range req.Tools {
		tool := map[string]any{
			"name":         normalizeToolName(t.Name),
			"description":  t.Description,
			"input_schema": json.RawMessage(t.Schema),
		}
		if t.CacheBreakpoint || i == len(req.Tools)-1 && req.Tools[i].CacheBreakpoint {
			tool["cache_control"] = map[string]any{"type": "ephemeral", "ttl": 3600}
		}
		toolsField = append(toolsField, tool)
	}

	body := map[string]interface{}{
		"model":      model,
		"messages":   msgs,
		"stream":     true,
		"max_tokens": 16384,
	}
	if systemField != nil {
		body["system"] = systemField
	}
	if len(toolsField) > 0 {
		body["tools"] = toolsField
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	// Inject thinking / extended thinking when budget > 0.
	// Requires the extended-thinking beta header (already in betaHeader).
	if req.ThinkingTokens > 0 {
		body["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": req.ThinkingTokens,
		}
		// Claude requires max_tokens > budget_tokens.
		// Reserve enough room for actual output beyond thinking.
		maxTok, _ := body["max_tokens"].(int)
		if maxTok <= req.ThinkingTokens {
			body["max_tokens"] = req.ThinkingTokens + 16384
		}
		// Temperature must be 1 when thinking is enabled.
		delete(body, "temperature")
	}
	return json.Marshal(body)
}

// streamClaudeSSE parses Anthropic SSE events and emits IR events.
// Handles text, tool_use, and thinking content blocks.
func streamClaudeSSE(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer body.Close()
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	var eventType string
	var curBlockType string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		default:
		}

		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		switch eventType {
		case "content_block_start":
			var ev struct {
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				curBlockType = ev.ContentBlock.Type
				switch ev.ContentBlock.Type {
				case "tool_use":
					out <- ir.Event{
						Kind:     ir.EvToolUseStart,
						ToolID:   ev.ContentBlock.ID,
						ToolName: ev.ContentBlock.Name,
					}
				case "thinking":
					// thinking block started — deltas follow
				case "text":
					// text block started — deltas follow
				}
			}
		case "content_block_delta":
			var ev struct {
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						out <- ir.Event{Kind: ir.EvTextDelta, Text: ev.Delta.Text}
					}
				case "input_json_delta":
					if ev.Delta.PartialJSON != "" {
						out <- ir.Event{Kind: ir.EvToolUseDelta, ToolDelta: []byte(ev.Delta.PartialJSON)}
					}
				case "thinking_delta":
					if ev.Delta.Thinking != "" {
						out <- ir.Event{Kind: ir.EvThinkingDelta, Text: ev.Delta.Thinking}
					}
				}
			}
		case "content_block_stop":
			if curBlockType == "tool_use" {
				out <- ir.Event{Kind: ir.EvToolUseEnd}
			}
			curBlockType = ""
		case "message_delta":
			var ev struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				if ev.Usage.OutputTokens > 0 {
					out <- ir.Event{Kind: ir.EvUsage, OutputTokens: ev.Usage.OutputTokens}
				}
				if ev.Delta.StopReason != "" {
					out <- ir.Event{Kind: ir.EvDone, FinishReason: ev.Delta.StopReason}
				}
			}
		case "message_start":
			var ev struct {
				Message struct {
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				out <- ir.Event{
					Kind:                ir.EvUsage,
					InputTokens:         ev.Message.Usage.InputTokens,
					CacheReadTokens:     ev.Message.Usage.CacheReadInputTokens,
					CacheCreationTokens: ev.Message.Usage.CacheCreationInputTokens,
				}
			}
		case "message_stop":
			return
		case "error":
			var ev struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				out <- ir.Event{Kind: ir.EvError, Err: errors.New(ev.Error.Message)}
			}
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		out <- ir.Event{Kind: ir.EvError, Err: err}
	}
}

// mapModel normalises incoming model ids to Anthropic API model strings.
func mapModel(m string) string {
	if m == "" {
		return "claude-sonnet-4-6"
	}
	overrides := map[string]string{
		"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
		"claude-opus-4-5":   "claude-opus-4-5-20251101",
		"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
	}
	if v, ok := overrides[m]; ok {
		return v
	}
	return m
}

func ptrVal(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func snippetB(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// injectAndSignBilling wraps billing.InjectBillingBlock for this package.
func injectAndSignBilling(body []byte) []byte {
	return billing.InjectBillingBlock(body)
}

// scrubHeaders removes proxy fingerprint headers before sending to Anthropic.
func scrubHeaders(h interface{ Del(string) }) {
	billing.ScrubInboundHeaders(h)
}

// toolNameMap normalizes third-party tool names to Claude Code TitleCase convention.
// Non-standard tool names are a fingerprint signal for third-party proxy detection.
var toolNameMap = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit",
	"glob": "Glob", "grep": "Grep", "ls": "LS",
	"webfetch": "WebFetch", "websearch": "WebSearch",
	"todowrite": "TodoWrite", "todoread": "TodoRead",
	"task": "Task", "taskread": "TaskRead",
	"notebookedit": "NotebookEdit",
	"question":     "Question", "skill": "Skill",
	"agent": "Agent",
}

func normalizeToolName(name string) string {
	if mapped, ok := toolNameMap[strings.ToLower(name)]; ok {
		return mapped
	}
	return name
}
