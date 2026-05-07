// Package gemini is a provider adapter for Google Gemini (gemini.google.com /
// Google One subscription via Gemini CLI Code Assist protocol).
// Real mode uses cloudcode-pa.googleapis.com with OAuth tokens obtained via
// the Google Gemini CLI OAuth flow.
package gemini

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
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const (
	ModeMock = "mock"
	ModeReal = "real"

	// geminiCodeAssistBase is the internal Gemini CLI endpoint that consumes
	// Google One / Gemini Advanced quota. Matches geminicli.GeminiCliBaseURL.
	geminiCodeAssistBase = "https://cloudcode-pa.googleapis.com"

	// geminiCLIUserAgent mimics the official Gemini CLI.
	geminiCLIUserAgent = "GeminiCLI/0.1.5 (Windows; AMD64)"
)

type Provider struct {
	Mode       string
	store      *store.Store
	resolver   *sessionResolver
	httpClient *http.Client
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func New(mode string) *Provider {
	if mode == "" {
		mode = ModeMock
	}
	return &Provider{
		Mode:       mode,
		resolver:   newSessionResolver(),
		httpClient: transport.Gemini,
	}
}

func (p *Provider) Name() string { return "gemini" }

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	switch p.Mode {
	case ModeMock:
		return p.invokeMock(ctx, acc, req)
	case ModeReal:
		return p.invokeReal(ctx, acc, req)
	}
	return nil, fmt.Errorf("unknown gemini mode: %s", p.Mode)
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
	_, err = p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, sec.RefreshToken)
	return err
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "gemini-2.5-pro", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-2.5-flash", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-2.5-flash-lite", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-3-pro-preview", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-3.1-pro-preview", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-3-flash-preview", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
		{ID: "gemini-3.1-flash-lite-preview", Available: true, ContextWindow: 1000000, SupportsTools: true, SupportsVision: true},
	}
	state := &domain.QuotaState{
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}

	// Try real quota detection via Google internal APIs.
	if p.Mode == ModeReal && p.store != nil {
		if err := p.fetchQuota(ctx, acc, state); err != nil {
			log.Printf("[gemini-discover] quota fetch failed: %v", err)
		}
	}
	return state, nil
}

// fetchQuota queries Gemini's internal APIs for real quota data.
// Step 1: loadCodeAssist → get project_id
// Step 2: retrieveUserQuota → get per-model remaining_fraction
func (p *Provider) fetchQuota(ctx context.Context, acc *domain.Account, state *domain.QuotaState) error {
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return err
	}
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, sec.RefreshToken)
	if err != nil {
		return err
	}

	// Step 1: loadCodeAssist to get project ID.
	loadBody := []byte(`{"metadata":{"ideType":"GEMINI_CLI","pluginType":"GEMINI"}}`)
	loadReq, err := http.NewRequestWithContext(ctx, "POST",
		geminiCodeAssistBase+"/v1internal:loadCodeAssist",
		bytes.NewReader(loadBody))
	if err != nil {
		return err
	}
	setGeminiCLIHeaders(loadReq.Header, info.AccessToken)

	loadResp, err := p.httpClient.Do(loadReq)
	if err != nil {
		return fmt.Errorf("loadCodeAssist: %w", err)
	}
	defer loadResp.Body.Close()
	loadRespBody, _ := io.ReadAll(loadResp.Body)
	if loadResp.StatusCode != 200 {
		return fmt.Errorf("loadCodeAssist %d: %s", loadResp.StatusCode, string(loadRespBody[:min(len(loadRespBody), 200)]))
	}

	// Extract project ID from cloudaicompanionProject field.
	projectField := gjson.GetBytes(loadRespBody, "cloudaicompanionProject").String()
	projectID := extractProjectID(projectField)

	// Step 2: retrieveUserQuota.
	quotaBody := []byte(`{}`)
	if projectID != "" {
		quotaBody = []byte(fmt.Sprintf(`{"project":"%s"}`, projectID))
	}
	quotaReq, err := http.NewRequestWithContext(ctx, "POST",
		geminiCodeAssistBase+"/v1internal:retrieveUserQuota",
		bytes.NewReader(quotaBody))
	if err != nil {
		return err
	}
	setGeminiCLIHeaders(quotaReq.Header, info.AccessToken)

	quotaResp, err := p.httpClient.Do(quotaReq)
	if err != nil {
		return fmt.Errorf("retrieveUserQuota: %w", err)
	}
	defer quotaResp.Body.Close()
	quotaRespBody, _ := io.ReadAll(quotaResp.Body)
	if quotaResp.StatusCode != 200 {
		return fmt.Errorf("retrieveUserQuota %d", quotaResp.StatusCode)
	}

	log.Printf("[gemini-quota] account=%s raw=%s", acc.ID, string(quotaRespBody[:min(len(quotaRespBody), 500)]))

	// Parse buckets and group by model category.
	type catEntry struct {
		remaining float64
		resetTime time.Time
	}
	categories := map[string]*catEntry{}

	gjson.GetBytes(quotaRespBody, "buckets").ForEach(func(_, bucket gjson.Result) bool {
		modelID := bucket.Get("modelId").String()
		remaining := bucket.Get("remainingFraction").Float()
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 1 {
			remaining = 1
		}
		cat := classifyGeminiModel(modelID)
		var resetAt time.Time
		if rt := bucket.Get("resetTime").String(); rt != "" {
			resetAt, _ = time.Parse(time.RFC3339Nano, rt)
		}
		e, ok := categories[cat]
		if !ok {
			categories[cat] = &catEntry{remaining: remaining, resetTime: resetAt}
		} else if remaining < e.remaining {
			e.remaining = remaining
			if !resetAt.IsZero() {
				e.resetTime = resetAt
			}
		}
		return true
	})

	if len(categories) == 0 {
		return nil
	}

	state.TierWindows = make(map[string]domain.QuotaWindow, len(categories))
	minRemaining := 1.0
	var minReset time.Time
	for name, e := range categories {
		utilization := (1.0 - e.remaining) * 100
		state.TierWindows[name] = domain.QuotaWindow{
			Limit: 100, Used: utilization, ResetAt: e.resetTime, Confidence: 1.0,
		}
		if e.remaining < minRemaining {
			minRemaining = e.remaining
			minReset = e.resetTime
		}
	}
	// LongWindow = overall worst category (general "how much is left" signal).
	state.LongWindow = domain.QuotaWindow{
		Limit: 100, Used: (1.0 - minRemaining) * 100,
		ResetAt: minReset, Confidence: 1.0,
	}

	log.Printf("[gemini-quota] account=%s categories=%d worst_used=%.0f%%", acc.ID, len(categories), (1.0-minRemaining)*100)
	return nil
}

func classifyGeminiModel(modelID string) string {
	lower := strings.ToLower(modelID)
	if strings.Contains(lower, "flash-lite") || strings.Contains(lower, "flash_lite") {
		return "gemini_flash_lite"
	}
	if strings.Contains(lower, "flash") {
		return "gemini_flash"
	}
	return "gemini_pro"
}

func extractProjectID(field string) string {
	// field is like "projects/123456/locations/..." → extract "123456"
	parts := strings.Split(field, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return field
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
		text := fmt.Sprintf("[mock gemini provider · account=%s · model=%s] Reply: %q", acc.ID, req.Model, preview)
		for _, w := range strings.Fields(text) {
			select {
			case <-ctx.Done():
				out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
				return
			case out <- ir.Event{Kind: ir.EvTextDelta, Text: w + " "}:
			}
			time.Sleep(15 * time.Millisecond)
		}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "STOP"}
	}()
	return out, nil
}

// ----- real mode (Gemini CLI Code Assist API) -----

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.store == nil {
		return nil, errors.New("gemini: store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, sec.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	model := mapGeminiModel(req.Model)
	body, err := buildCodeAssistBody(req, model)
	if err != nil {
		return nil, err
	}

	// Gemini CLI Code Assist streaming endpoint.
	endpoint := fmt.Sprintf("%s/v1internal/chat:sendMessageStream", geminiCodeAssistBase)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setGeminiCLIHeaders(httpReq.Header, info.AccessToken)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post gemini codeassist: %w", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippetG(b))
	}

	out := make(chan ir.Event, 32)
	go streamGeminiCodeAssist(ctx, resp.Body, out)
	return out, nil
}

func setGeminiCLIHeaders(h http.Header, accessToken string) {
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", geminiCLIUserAgent)
	h.Set("Accept", "application/json")
	h.Set("X-Goog-Api-Client", "google-genai-sdk/1.41.0 gl-node/v22.19.0")
}

// buildCodeAssistBody converts an IR request to the Gemini Code Assist JSON shape.
func buildCodeAssistBody(req *ir.Request, model string) ([]byte, error) {
	type inlineData struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	type part struct {
		Text       string      `json:"text,omitempty"`
		InlineData *inlineData `json:"inlineData,omitempty"`
	}
	type content struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}

	var contents []content
	var systemText string
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleSystem:
			for _, p := range m.Parts {
				if p.Kind == ir.PartText {
					systemText += p.Text
				}
			}
		case ir.RoleUser, ir.RoleAssistant:
			role := "user"
			if m.Role == ir.RoleAssistant {
				role = "model"
			}
			var parts []part
			for _, p := range m.Parts {
				if p.Kind == ir.PartText && p.Text != "" {
					parts = append(parts, part{Text: p.Text})
				} else if p.Kind == ir.PartImage && (len(p.ImageBytes) > 0 || p.ImageURL != "") {
					// Gemini inline_data format
					parts = append(parts, part{
						InlineData: &inlineData{
							MimeType: p.ImageMedia,
							Data:     string(p.ImageBytes),
						},
					})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, content{Role: role, Parts: parts})
			}
		}
	}

	body := map[string]interface{}{
		"model":    model,
		"contents": contents,
	}
	if req.System != "" {
		systemText = req.System
	}
	if systemText != "" {
		body["systemInstruction"] = map[string]interface{}{
			"parts": []part{{Text: systemText}},
		}
	}
	if req.MaxTokens > 0 {
		body["generationConfig"] = map[string]interface{}{
			"maxOutputTokens": req.MaxTokens,
		}
	}
	return json.Marshal(body)
}

// streamGeminiCodeAssist parses Gemini's newline-delimited JSON streaming response.
func streamGeminiCodeAssist(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer body.Close()
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "[" || line == "]" || line == "," {
			continue
		}
		// Strip leading comma for NDJSON arrays.
		line = strings.TrimPrefix(line, ",")

		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
				ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		for _, cand := range chunk.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					out <- ir.Event{Kind: ir.EvTextDelta, Text: p.Text}
				}
			}
			if cand.FinishReason != "" && cand.FinishReason != "FINISH_REASON_UNSPECIFIED" {
				cached := chunk.UsageMetadata.CachedContentTokenCount
				out <- ir.Event{
					Kind:            ir.EvUsage,
					InputTokens:     chunk.UsageMetadata.PromptTokenCount - cached,
					OutputTokens:    chunk.UsageMetadata.CandidatesTokenCount + chunk.UsageMetadata.ThoughtsTokenCount,
					CacheReadTokens: cached,
				}
				out <- ir.Event{Kind: ir.EvDone, FinishReason: cand.FinishReason}
				return
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		out <- ir.Event{Kind: ir.EvError, Err: err}
	}
}

func mapGeminiModel(m string) string {
	if m == "" {
		return "gemini-2.5-pro"
	}
	return m
}

func snippetG(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
