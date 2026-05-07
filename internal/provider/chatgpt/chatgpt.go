// Package chatgpt is the provider adapter for chatgpt.com (OpenAI subscription
// web). MVP supports two modes:
//
//   - mock: deterministic test response (default)
//   - real: actual reverse-engineered call to /backend-api/conversation
//
// Real mode handles two credential formats (see session.go) and parses the
// JSON-Patch SSE delta protocol (see stream.go) into IR events.
package chatgpt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const (
	ModeMock = "mock"
	ModeReal = "real"
)

type Provider struct {
	Mode       string
	store      *store.Store
	resolver   *sessionResolver
	httpClient *http.Client
}

// SetStore wires the credential store. Required for ModeReal; harmless in mock.
func (p *Provider) SetStore(s *store.Store) { p.store = s }

// SetRefreshFunc plumbs the OAuth refresh callback to the session resolver.
func (p *Provider) SetRefreshFunc(f func(ctx context.Context, refreshToken string) (string, string, string, int, error)) {
	p.resolver.SetRefreshFunc(f)
}

func New(mode string) *Provider {
	if mode == "" {
		mode = ModeMock
	}
	return &Provider{
		Mode:       mode,
		resolver:   newSessionResolver(),
		httpClient: transport.Codex,
	}
}

func (p *Provider) Name() string { return "chatgpt" }

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	switch p.Mode {
	case ModeMock:
		return p.invokeMock(ctx, acc, req)
	case ModeReal:
		return p.invokeReal(ctx, acc, req)
	}
	return nil, fmt.Errorf("unknown chatgpt mode: %s", p.Mode)
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
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, acc.UA)
	if err != nil {
		return err
	}
	_, err = p.fetchWhamUsage(ctx, acc, info.AccessToken, info.AccountID)
	return err
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	// Model lists sourced from CPA models.json (github.com/router-for-me/models).
	// Codex plan tiers: free/team share the same base set; plus/pro add spark model.
	codexBase := []domain.ModelCapability{
		{ID: "gpt-5.2", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-5.3-codex", Available: true, ContextWindow: 128000, SupportsTools: true},
		{ID: "gpt-5.4", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-5.4-mini", Available: true, ContextWindow: 128000, SupportsTools: true},
		{ID: "gpt-5.5", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-image-2", Available: true, ContextWindow: 128000, SupportsVision: true},
		{ID: "codex-auto-review", Available: true, ContextWindow: 128000, SupportsTools: true},
	}
	codexSparkExtra := domain.ModelCapability{
		ID: "gpt-5.3-codex-spark", Available: true, ContextWindow: 128000, SupportsTools: true,
	}

	if p.Mode == ModeMock {
		models := append([]domain.ModelCapability{}, codexBase...)
		models = append(models, codexSparkExtra)
		return &domain.QuotaState{
			ShortWindow:      domain.QuotaWindow{Limit: 80, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
			LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
			DiscoveredModels: models,
			LastDiscoveryAt:  time.Now(),
		}, nil
	}
	if p.store == nil {
		return nil, errors.New("store not wired")
	}
	log.Printf("[chatgpt-discover] account=%s starting discover", acc.ID)
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		log.Printf("[chatgpt-discover] account=%s GetAccountSecret error: %v", acc.ID, err)
		return nil, err
	}
	log.Printf("[chatgpt-discover] account=%s secret loaded, session_token_len=%d, ua=%q", acc.ID, len(sec.SessionToken), acc.UA)
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, acc.UA)
	if err != nil {
		log.Printf("[chatgpt-discover] account=%s Resolve error: %v", acc.ID, err)
		// Detect ban from session error
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "banned") || strings.Contains(errMsg, "deactivated") || strings.Contains(errMsg, "suspended") {
			acc.State = domain.StateBanned
			log.Printf("[chatgpt-discover] account=%s BANNED detected during session resolve", acc.ID)
		}
		return nil, err
	}
	log.Printf("[chatgpt-discover] account=%s resolved: access_token_len=%d, account_id=%q, plan=%q, expires=%v",
		acc.ID, len(info.AccessToken), info.AccountID, info.PlanType, info.Expires)

	models := append([]domain.ModelCapability{}, codexBase...)
	tier := acc.PlanTier
	if info.PlanType != "" {
		tier = info.PlanType
	}
	if tier == "plus" || tier == "pro" || tier == "team" {
		models = append(models, codexSparkExtra)
	}

	quotaState, err := p.fetchConversationLimit(ctx, acc, info.AccessToken, info.AccountID)
	if err != nil {
		return nil, err
	}
	quotaState.DiscoveredModels = models
	quotaState.PlanTier = tier
	quotaState.LastDiscoveryAt = time.Now()
	log.Printf("[chatgpt-discover] account=%s discover done: tier=%s 5h=%.1f/%.1f(conf=%.1f) 7d=%.1f/%.1f(conf=%.1f) models=%d",
		acc.ID, tier,
		quotaState.ShortWindow.Used, quotaState.ShortWindow.Limit, quotaState.ShortWindow.Confidence,
		quotaState.LongWindow.Used, quotaState.LongWindow.Limit, quotaState.LongWindow.Confidence,
		len(quotaState.DiscoveredModels))
	return quotaState, nil
}

// fetchConversationLimit tries two endpoints to get real quota:
//  1. /backend-api/wham/usage (Codex CLI style, current)
//  2. /backend-api/conversation_limit (ChatGPT web style, legacy fallback)
func (p *Provider) fetchConversationLimit(ctx context.Context, acc *domain.Account, accessToken, accountID string) (*domain.QuotaState, error) {
	state := &domain.QuotaState{
		ShortWindow: domain.QuotaWindow{ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:  domain.QuotaWindow{ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
	}

	// Try wham/usage first (Codex CLI endpoint)
	whamState, err := p.fetchWhamUsage(ctx, acc, accessToken, accountID)
	if err == nil {
		*state = *whamState
		log.Printf("[chatgpt-quota] wham/usage ok: 5h=%.0f/%.0f 7d=%.0f/%.0f",
			state.ShortWindow.Used, state.ShortWindow.Limit,
			state.LongWindow.Used, state.LongWindow.Limit)
		return state, nil
	}
	if isBannedError(err) {
		return nil, err
	}
	log.Printf("[chatgpt-quota] wham/usage failed, trying conversation_limit: %v", err)
	// Fallback to conversation_limit (legacy ChatGPT web endpoint)
	if err := p.fetchLegacyConversationLimit(ctx, state, acc, accessToken, accountID); err != nil {
		return nil, err
	}
	log.Printf("[chatgpt-quota] conversation_limit: 5h=%.0f/%.0f 7d=%.0f/%.0f",
		state.ShortWindow.Used, state.ShortWindow.Limit,
		state.LongWindow.Used, state.LongWindow.Limit)
	return state, nil
}

// fetchWhamUsage calls /backend-api/wham/usage (Codex CLI's rate limit endpoint).
// Response format:
//
//	{"rate_limit":{"used_percent":0.25,"window_minutes":300,"resets_at":1714...},
//	 "additional_rate_limits":[{"limit_name":"weekly","rate_limit":{"used_percent":0.1,"window_minutes":10080,"resets_at":...}}],
//	 "credits":{"has_credits":true,"unlimited":false,"balance":"$4.20"},
//	 "plan_type":"plus"}
func (p *Provider) fetchWhamUsage(ctx context.Context, acc *domain.Account, accessToken, accountID string) (*domain.QuotaState, error) {
	state := &domain.QuotaState{
		ShortWindow: domain.QuotaWindow{ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:  domain.QuotaWindow{ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
		TierWindows: make(map[string]domain.QuotaWindow),
	}
	ua := codexUA(acc)
	log.Printf("[chatgpt-quota] wham/usage: starting, account_id=%q, access_token_len=%d", accountID, len(accessToken))
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	req.Header["originator"] = []string{"codex_cli_rs"}
	if accountID != "" {
		req.Header["ChatGPT-Account-ID"] = []string{accountID}
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wham/usage request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		if schedulerClassifyBanned(resp.StatusCode, body) {
			return nil, fmt.Errorf("account banned: wham/usage %d: %s", resp.StatusCode, snippet(body))
		}
		return nil, fmt.Errorf("wham/usage %d: %s", resp.StatusCode, snippet(body))
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("wham/usage invalid json: %s", snippet(body))
	}
	parseWhamUsage(body, state)
	if state.ShortWindow.Confidence <= 0.5 && state.LongWindow.Confidence <= 0.5 {
		return nil, fmt.Errorf("wham/usage missing quota windows")
	}
	log.Printf("[chatgpt-quota] wham/usage: final short={used=%.1f lim=%.1f conf=%.1f} long={used=%.1f lim=%.1f conf=%.1f} plan=%s tiers=%d",
		state.ShortWindow.Used, state.ShortWindow.Limit, state.ShortWindow.Confidence,
		state.LongWindow.Used, state.LongWindow.Limit, state.LongWindow.Confidence,
		state.PlanTier, len(state.TierWindows))
	return state, nil
}

func parseWhamUsage(body []byte, state *domain.QuotaState) {
	root := gjson.ParseBytes(body)

	// Parse primary window (5h)
	primary := root.Get("rate_limit.primary_window")
	primaryWindow, primaryWinSec, primaryOK := parseWhamWindow(primary, 5*time.Hour)
	if primaryOK {
		state.ShortWindow = primaryWindow
	} else if legacy := root.Get("rate_limit"); legacy.Get("used_percent").Exists() {
		legacyWindow, legacyWinSec, ok := parseWhamWindow(legacy, 5*time.Hour)
		if ok {
			state.ShortWindow = legacyWindow
			primaryWinSec = legacyWinSec
			primaryOK = true
		}
	}

	// Parse secondary window (7d)
	secondary := root.Get("rate_limit.secondary_window")
	secondaryWindow, secondaryWinSec, secondaryOK := parseWhamWindow(secondary, 7*24*time.Hour)
	if secondaryOK {
		state.LongWindow = secondaryWindow
	}

	// Store plan type
	planType := root.Get("plan_type").String()
	if planType != "" {
		state.PlanTier = planType
	}

	// Populate TierWindows with dynamic names based on window duration.
	if state.TierWindows == nil {
		state.TierWindows = make(map[string]domain.QuotaWindow)
	}
	if primaryOK {
		state.TierWindows[windowSecsToTierName(primaryWinSec)] = state.ShortWindow
	}
	if secondaryOK {
		state.TierWindows[windowSecsToTierName(secondaryWinSec)] = state.LongWindow
	}

	// Parse additional_rate_limits when it is either an array or an object.
	root.Get("additional_rate_limits").ForEach(func(k, v gjson.Result) bool {
		parseWhamExtraRateLimit(k.String(), v, state)
		return true
	})

	// Codex-Manager also records arbitrary *_rate_limit siblings. Preserve them
	// as named tier windows so model-specific or feature-specific caps are visible.
	root.ForEach(func(k, v gjson.Result) bool {
		name := k.String()
		if name != "rate_limit" && strings.HasSuffix(name, "_rate_limit") {
			parseWhamExtraRateLimit(name, v, state)
		}
		return true
	})

	// Parse credits field.
	if credits := root.Get("credits"); credits.Exists() {
		state.ExtraUsage = &domain.ExtraUsage{
			IsEnabled: credits.Get("has_credits").Bool(),
			Currency:  credits.Get("balance").String(),
		}
	}
}

func parseWhamExtraRateLimit(sourceName string, raw gjson.Result, state *domain.QuotaState) {
	if !raw.Exists() {
		return
	}
	if state.TierWindows == nil {
		state.TierWindows = make(map[string]domain.QuotaWindow)
	}
	baseName := whamRateLimitName(sourceName, raw)
	rl := raw.Get("rate_limit")
	if !rl.Exists() {
		rl = raw
	}

	primary := rl.Get("primary_window")
	secondary := rl.Get("secondary_window")
	parsedNested := false
	if primary.Exists() {
		if window, winSec, ok := parseWhamWindow(primary, 0); ok {
			name := whamWindowTierName(baseName, "primary", winSec)
			state.TierWindows[name] = window
			parsedNested = true
		}
	}
	if secondary.Exists() {
		if window, winSec, ok := parseWhamWindow(secondary, 0); ok {
			name := whamWindowTierName(baseName, "secondary", winSec)
			state.TierWindows[name] = window
			parsedNested = true
		}
	}
	if parsedNested {
		return
	}

	window, winSec, ok := parseWhamWindow(rl, 0)
	if !ok {
		return
	}
	name := firstNonEmpty(baseName, windowSecsToTierName(winSec))
	if name == "" {
		name = "unknown"
	}
	state.TierWindows[name] = window
}

func whamWindowTierName(baseName, suffix string, winSec int64) string {
	baseName = strings.TrimSpace(baseName)
	if baseName == "" {
		return windowSecsToTierName(winSec)
	}
	return baseName + "_" + suffix
}

func whamRateLimitName(sourceName string, raw gjson.Result) string {
	sourceName = strings.TrimSpace(sourceName)
	if sourceName == "" || isNumericString(sourceName) {
		sourceName = ""
	}
	sourceName = strings.TrimSuffix(sourceName, "_rate_limit")
	return firstNonEmpty(
		raw.Get("limit_name").String(),
		raw.Get("metered_feature").String(),
		raw.Get("limit_id").String(),
		sourceName,
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isNumericString(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseWhamWindow(raw gjson.Result, defaultWindow time.Duration) (domain.QuotaWindow, int64, bool) {
	if !raw.Exists() || !raw.Get("used_percent").Exists() {
		return domain.QuotaWindow{}, 0, false
	}
	usedPct := raw.Get("used_percent").Float()
	winSec := raw.Get("limit_window_seconds").Int()
	if winSec <= 0 {
		if mins := raw.Get("window_minutes").Int(); mins > 0 {
			winSec = mins * 60
		}
	}
	resetAt := raw.Get("reset_at").Int()
	if resetAt <= 0 {
		resetAt = raw.Get("resets_at").Int()
	}
	var resetTime time.Time
	if resetAt > 0 {
		resetTime = time.Unix(resetAt, 0)
	} else if after := raw.Get("reset_after_seconds").Int(); after > 0 {
		resetTime = time.Now().Add(time.Duration(after) * time.Second)
	} else if defaultWindow > 0 {
		resetTime = time.Now().Add(defaultWindow)
	}
	if winSec <= 0 && defaultWindow > 0 {
		winSec = int64(defaultWindow / time.Second)
	}
	return domain.QuotaWindow{
		Limit:      100,
		Used:       usedPct,
		ResetAt:    resetTime,
		Confidence: 1.0,
	}, winSec, true
}

func windowSecsToTierName(secs int64) string {
	switch secs {
	case 18000:
		return "five_hour"
	case 604800:
		return "seven_day"
	default:
		hours := secs / 3600
		if hours >= 24 {
			return fmt.Sprintf("%d_day", hours/24)
		}
		if hours > 0 {
			return fmt.Sprintf("%d_hour", hours)
		}
		return "unknown"
	}
}

// fetchLegacyConversationLimit calls /backend-api/conversation_limit (legacy).
func (p *Provider) fetchLegacyConversationLimit(ctx context.Context, state *domain.QuotaState, acc *domain.Account, accessToken, accountID string) error {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/backend-api/conversation_limit", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", codexUA(acc))
	req.Header.Set("Accept", "application/json")
	req.Header["originator"] = []string{"codex_cli_rs"}
	if accountID != "" {
		req.Header["ChatGPT-Account-ID"] = []string{accountID}
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		if schedulerClassifyBanned(resp.StatusCode, body) {
			return fmt.Errorf("account banned: conversation_limit %d: %s", resp.StatusCode, snippet(body))
		}
		return fmt.Errorf("conversation_limit %d: %s", resp.StatusCode, snippet(body))
	}

	parse := func(key string) (limit, remaining int64, resetAt time.Time) {
		limit = gjson.GetBytes(body, key+".limit").Int()
		remaining = gjson.GetBytes(body, key+".remaining").Int()
		if rt := gjson.GetBytes(body, key+".reset_time_utc").String(); rt != "" {
			resetAt, _ = time.Parse(time.RFC3339, rt)
		}
		if resetAt.IsZero() {
			resetAt = time.Now().Add(5 * time.Hour)
		}
		return
	}

	shortLimit, shortRem, shortReset := parse("message_cap_ffp")
	longLimit, longRem, longReset := parse("message_cap_ffp_7d")

	if shortLimit > 0 {
		state.ShortWindow = domain.QuotaWindow{
			Limit:      float64(shortLimit),
			Used:       float64(shortLimit - shortRem),
			ResetAt:    shortReset,
			Confidence: 1.0,
		}
	}
	if longLimit > 0 {
		state.LongWindow = domain.QuotaWindow{
			Limit:      float64(longLimit),
			Used:       float64(longLimit - longRem),
			ResetAt:    longReset,
			Confidence: 1.0,
		}
	}
	return nil
}

func codexUA(acc *domain.Account) string {
	if acc != nil && acc.UA != "" {
		return acc.UA
	}
	return "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
}

func schedulerClassifyBanned(status int, body []byte) bool {
	return scheduler.ClassifyError(status, string(body), nil) == domain.ErrBanned
}

func isBannedError(err error) bool {
	return scheduler.ClassifyError(0, "", err) == domain.ErrBanned
}

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
		response := fmt.Sprintf(
			"[mock chatgpt provider · account=%s · model=%s]\n\nYou said: %q\n\nThis is a deterministic mock response from the LLM-pool gateway. Replace with real ChatGPT integration when subscription credentials are captured.",
			acc.ID, req.Model, preview,
		)
		words := strings.Fields(response)
		for _, w := range words {
			select {
			case <-ctx.Done():
				out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
				return
			case out <- ir.Event{Kind: ir.EvTextDelta, Text: w + " "}:
			}
			time.Sleep(15 * time.Millisecond)
		}
		out <- ir.Event{Kind: ir.EvUsage, InputTokens: 50, OutputTokens: len(words)}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
}

// ----- Real mode (Codex Responses API) -----
//
// We POST to https://chatgpt.com/backend-api/codex/responses, which:
//   - consumes the *Codex* quota bucket (separate from regular ChatGPT chat
//     limits) — primary 5h window, secondary 7d window, both reported in
//     X-Codex-* response headers
//   - accepts the OpenAI Responses API request shape (input array, instructions,
//     reasoning, tools, ...) and streams response.* SSE events back
//   - bypasses the chatgpt.com web Turnstile / sentinel layer entirely
//
// The Plus-account-allowed model slugs come from
// /backend-api/codex/models?client_version=0.45.0 (e.g. "gpt-5.2").

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.store == nil {
		return nil, errors.New("chatgpt: store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, acc.UA)
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	model := mapToCodexModelSlug(req.Model)
	body, err := buildResponsesBody(req, model)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/codex/responses",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ua := acc.UA
	if ua == "" {
		ua = "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
	}
	httpReq.Header.Set("Authorization", "Bearer "+info.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header["OpenAI-Beta"] = []string{"responses=experimental"}
	httpReq.Header["originator"] = []string{"codex_cli_rs"}
	httpReq.Header["ChatGPT-Account-ID"] = []string{info.AccountID}
	setCodexSessionHeaders(httpReq.Header, body)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post codex/responses: %w", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippet(b))
	}

	out := make(chan ir.Event, 32)
	go streamResponsesSSE(ctx, resp.Body, out)
	return out, nil
}

// InvokeRaw sends a raw Responses API JSON body to the upstream Codex endpoint
// and returns the raw SSE response stream for passthrough proxying.
func (p *Provider) InvokeRaw(ctx interface{}, accountID string, body []byte) (io.ReadCloser, int, error) {
	rctx, ok := ctx.(context.Context)
	if !ok {
		return nil, 0, errors.New("invalid context")
	}
	if p.store == nil {
		return nil, 0, errors.New("chatgpt: store not wired")
	}
	sec, err := p.store.GetAccountSecret(rctx, accountID)
	if err != nil {
		return nil, 0, fmt.Errorf("get secret: %w", err)
	}
	acc, _ := p.store.ListAccounts(rctx, "")
	var ua string
	for _, a := range acc {
		if a.ID == accountID {
			ua = a.UA
			break
		}
	}
	info, err := p.resolver.Resolve(rctx, accountID, sec.SessionToken, ua)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve session: %w", err)
	}

	if ua == "" {
		ua = "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
	}

	httpReq, err := http.NewRequestWithContext(rctx, "POST",
		"https://chatgpt.com/backend-api/codex/responses",
		bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+info.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header["OpenAI-Beta"] = []string{"responses=experimental"}
	httpReq.Header["originator"] = []string{"codex_cli_rs"}
	httpReq.Header["ChatGPT-Account-ID"] = []string{info.AccountID}
	setCodexSessionHeaders(httpReq.Header, body)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("post codex/responses: %w", err)
	}
	return resp.Body, resp.StatusCode, nil
}

// buildResponsesBody renders an IR request into the Responses API JSON shape.
// The conversation history is passed as the `input` array; each turn becomes
// a {"type":"message","role":"...","content":[{"type":"input_text","text":...}]} entry.
// buildResponsesBody converts an IR request to the OpenAI Responses API format
// used by chatgpt.com/backend-api/codex/responses. Includes full tool support
// so Claude Code plan mode / multi-agent / tool_use works via GPT backend.
func buildResponsesBody(req *ir.Request, model string) ([]byte, error) {
	// --- input items ---
	var items []any
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleSystem:
			continue // injected into instructions below
		case ir.RoleUser:
			// Flush tool results first (they reference the preceding function_calls),
			// then append any text content as a user message.
			var textParts []any
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					textParts = append(textParts, map[string]any{"type": "input_text", "text": p.Text})
				case ir.PartImage:
					url := p.ImageURL
					if url == "" && len(p.ImageBytes) > 0 {
						url = "data:" + p.ImageMedia + ";base64," + string(p.ImageBytes)
					}
					if url != "" {
						textParts = append(textParts, map[string]any{"type": "input_image", "image_url": url})
					}
				case ir.PartToolResult:
					var content string
					if len(p.ToolResultBytes) > 0 {
						content = string(p.ToolResultBytes)
					}
					items = append(items, map[string]any{
						"type":    "function_call_output",
						"call_id": p.ToolResultID,
						"output":  content,
					})
				}
			}
			if len(textParts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": textParts})
			}
		case ir.RoleAssistant:
			// Maintain correct interleaving: text before tool calls.
			// Flush accumulated text parts before each function_call so GPT
			// sees the assistant's reasoning context before the tool invocation.
			var textParts []any
			flushText := func() {
				if len(textParts) > 0 {
					items = append(items, map[string]any{"type": "message", "role": "assistant", "content": textParts})
					textParts = nil
				}
			}
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					textParts = append(textParts, map[string]any{"type": "output_text", "text": p.Text})
				case ir.PartToolUse:
					flushText()
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   p.ToolUseID,
						"name":      p.ToolUseName,
						"arguments": string(p.ToolUseInput),
					})
				}
			}
			flushText()
		default:
			for _, p := range m.Parts {
				if p.Kind == ir.PartText && p.Text != "" {
					items = append(items, map[string]any{
						"type": "message", "role": string(m.Role),
						"content": []any{map[string]any{"type": "input_text", "text": p.Text}},
					})
				}
			}
		}
	}

	// --- instructions (system prompt) ---
	instructions := req.System
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	// --- tools (function definitions) ---
	var tools []any
	for _, t := range req.Tools {
		var schema any
		_ = json.Unmarshal(t.Schema, &schema)
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  schema,
			"strict":      false,
		})
	}

	body := map[string]any{
		"model":               model,
		"input":               items,
		"instructions":        instructions,
		"reasoning":           map[string]string{"effort": pickReasoningEffort(req)},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
		"parallel_tool_calls": true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	// Codex Responses API does NOT support max_output_tokens — omit.
	return json.Marshal(body)
}

// mapToCodexModelSlug converts incoming model ids (gpt-4o, gpt-4o-mini, ...)
// into the slug ChatGPT-Codex actually accepts for Plus/Pro accounts.
// The accepted slugs come from /backend-api/codex/models — at time of writing
// "gpt-5.2" is the canonical default for Plus.
func mapToCodexModelSlug(m string) string {
	switch m {
	case "":
		return "gpt-5.2"
	case "gpt-5.2", "gpt-5.3", "gpt-5":
		return m
	case "gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4-turbo", "gpt-4":
		// these aren't supported on the codex/responses path for ChatGPT
		// accounts; transparently upgrade to the default Codex model.
		return "gpt-5.2"
	case "o1", "o1-mini", "o3", "o3-mini", "o4-mini":
		return "gpt-5.2"
	}
	return m
}

// pickReasoningEffort maps the IR reasoning effort to the Codex Responses API
// "effort" field. Codex supports: low | medium | high | xhigh (highest).
func pickReasoningEffort(req *ir.Request) string {
	switch strings.ToLower(req.ReasoningEffort) {
	case "low", "minimal":
		return "low"
	case "medium", "normal":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "maximum":
		return "xhigh"
	case "auto", "":
		return "high"
	}
	return "high"
}

// buildConversationBody renders an IR request into the JSON ChatGPT expects.
func buildConversationBody(req *ir.Request) ([]byte, error) {
	type msgContent struct {
		ContentType string   `json:"content_type"`
		Parts       []string `json:"parts"`
	}
	type msgAuthor struct {
		Role string `json:"role"`
	}
	type message struct {
		ID       string     `json:"id"`
		Author   msgAuthor  `json:"author"`
		Content  msgContent `json:"content"`
		Metadata struct {
			SerializationMetadata struct {
				CustomSymbolOffsets []any `json:"custom_symbol_offsets"`
			} `json:"serialization_metadata"`
		} `json:"metadata"`
	}
	model := req.Model
	if model == "" {
		model = "gpt-4o"
	}
	chatgptModel := mapToChatgptModelSlug(model)

	msgs := []message{}
	// We forward only the most recent user message; ChatGPT keeps history
	// server-side via parent_message_id when we use the same conversation,
	// but for the MVP we send the current turn and let the model see the
	// full inline history through the system context that the encoder built.
	// Actually ChatGPT expects ONE user message in the body — history must
	// be replayed via the conversation_id. To keep MVP simple, we collapse
	// the full conversation into the single user turn.
	var collapsed strings.Builder
	if req.System != "" {
		collapsed.WriteString(req.System)
		collapsed.WriteString("\n\n")
	}
	for i, m := range req.Messages {
		if i == len(req.Messages)-1 && m.Role == ir.RoleUser {
			break
		}
		var role string
		switch m.Role {
		case ir.RoleUser:
			role = "User"
		case ir.RoleAssistant:
			role = "Assistant"
		default:
			role = string(m.Role)
		}
		for _, p := range m.Parts {
			if p.Kind == ir.PartText && p.Text != "" {
				collapsed.WriteString(role + ": " + p.Text + "\n")
			}
		}
	}
	// Last user message (or whatever the final message is)
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("empty messages")
	}
	last := req.Messages[len(req.Messages)-1]
	for _, p := range last.Parts {
		if p.Kind == ir.PartText && p.Text != "" {
			collapsed.WriteString(p.Text)
		}
	}

	msgs = append(msgs, message{
		ID:      uuid4(),
		Author:  msgAuthor{Role: "user"},
		Content: msgContent{ContentType: "text", Parts: []string{collapsed.String()}},
	})

	body := map[string]any{
		"action":                        "next",
		"messages":                      msgs,
		"parent_message_id":             uuid4(),
		"model":                         chatgptModel,
		"timezone_offset_min":           -480,
		"suggestions":                   []any{},
		"history_and_training_disabled": true,
		"conversation_mode":             map[string]string{"kind": "primary_assistant"},
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_nulligen":                false,
		"force_rate_limit":              false,
		"reset_rate_limits":             false,
		"websocket_request_id":          uuid4(),
		"system_hints":                  []any{},
		"force_use_sse":                 true,
		"conversation_origin":           nil,
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 30,
			"page_height":       900,
			"page_width":        1440,
			"pixel_ratio":       2,
			"screen_height":     900,
			"screen_width":      1440,
		},
		"paragen_stream_type_override":         nil,
		"paragen_cot_summary_display_override": "allow",
		"supports_buffering":                   true,
	}
	return json.Marshal(body)
}

// mapToChatgptModelSlug converts API-style model ids (gpt-4o, o1-mini, ...)
// to the slug ChatGPT web expects (gpt-4o, o1-mini-2024-09-12, ...).
func mapToChatgptModelSlug(m string) string {
	switch m {
	case "gpt-4o":
		return "gpt-4o"
	case "gpt-4o-mini":
		return "gpt-4o-mini"
	case "o1-mini":
		return "o1-mini"
	case "o1":
		return "o1"
	case "gpt-4-turbo":
		return "gpt-4"
	}
	// pass through for newer models we don't know about yet
	return m
}

const maxCodexSessionHeaderLen = 512

func setCodexSessionHeaders(h http.Header, responsesBody []byte) {
	sessionID, threadID := codexSessionHeadersFromResponsesBody(responsesBody)
	h["session_id"] = []string{sessionID}
	if threadID != "" {
		h["thread_id"] = []string{threadID}
		h["x-client-request-id"] = []string{threadID}
	}
}

func codexSessionHeadersFromResponsesBody(body []byte) (sessionID, threadID string) {
	cacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if validCodexSessionHeaderValue(cacheKey) {
		return cacheKey, cacheKey
	}
	return uuid4(), ""
}

func validCodexSessionHeaderValue(v string) bool {
	if v == "" || len(v) > maxCodexSessionHeaderLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

func uuid4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}
