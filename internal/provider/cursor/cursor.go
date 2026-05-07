package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
	"github.com/tidwall/gjson"
)

const (
	codexBaseURL = "https://chatgpt.com/backend-api/codex"
	userAgent    = "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
)

type Provider struct {
	Mode       string
	store      *store.Store
	httpClient *http.Client
}

func New(mode string) *Provider {
	if mode == "" {
		mode = "mock"
	}
	return &Provider{
		Mode:       mode,
		httpClient: transport.ForProvider("chatgpt.com", transport.Options{Timeout: 120 * time.Second}),
	}
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func (p *Provider) Name() string { return "cursor" }

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.Mode == "mock" {
		return p.invokeMock(ctx, acc, req)
	}
	return p.invokeReal(ctx, acc, req)
}

func (p *Provider) Probe(ctx context.Context, acc *domain.Account) error {
	if p.Mode == "mock" {
		return nil
	}
	return p.probeReal(ctx, acc)
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	if p.Mode == "mock" {
		return p.discoverMock()
	}
	return p.discoverReal(ctx, acc)
}

// ---------------------------------------------------------------------------
// real mode
// ---------------------------------------------------------------------------

func (p *Provider) getToken(ctx context.Context, accID string) (string, error) {
	sec, err := p.store.GetAccountSecret(ctx, accID)
	if err != nil {
		return "", fmt.Errorf("cursor: get secret: %w", err)
	}
	if sec.SessionToken == "" {
		return "", errors.New("cursor: session token is empty")
	}
	return sec.SessionToken, nil
}

func setAuthHeaders(r *http.Request, token string) {
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("User-Agent", userAgent)
	r.Header.Set("OpenAI-Beta", "responses=experimental")
	r.Header.Set("originator", "codex_cli_rs")
}

// buildResponsesBody converts an ir.Request into the OpenAI Responses API body.
func buildResponsesBody(req *ir.Request) ([]byte, error) {
	type contentPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type inputMsg struct {
		Type    string        `json:"type"`
		Role    string        `json:"role"`
		Content []contentPart `json:"content"`
	}

	input := make([]inputMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := string(m.Role)
		var parts []contentPart
		for _, p := range m.Parts {
			if p.Kind == ir.PartText && p.Text != "" {
				parts = append(parts, contentPart{Type: "input_text", Text: p.Text})
			}
		}
		if len(parts) == 0 {
			continue
		}
		input = append(input, inputMsg{
			Type:    "message",
			Role:    role,
			Content: parts,
		})
	}

	body := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": true,
	}
	if req.System != "" {
		body["instructions"] = req.System
	}

	return json.Marshal(body)
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	token, err := p.getToken(ctx, acc.ID)
	if err != nil {
		return nil, err
	}

	bodyBytes, err := buildResponsesBody(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal body: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/responses", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	setAuthHeaders(httpReq, token)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cursor: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("cursor: HTTP %d: %s", resp.StatusCode, string(b))
	}

	out := make(chan ir.Event, 32)
	go p.streamSSE(ctx, resp.Body, out)
	return out, nil
}

func (p *Provider) streamSSE(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer close(out)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]

		if data == "[DONE]" {
			return
		}

		evType := gjson.Get(data, "type").String()

		switch evType {
		case "response.output_text.delta":
			delta := gjson.Get(data, "delta").String()
			if delta != "" {
				select {
				case <-ctx.Done():
					out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
					return
				case out <- ir.Event{Kind: ir.EvTextDelta, Text: delta}:
				}
			}

		case "response.completed", "response.done":
			usage := gjson.Get(data, "response.usage")
			if usage.Exists() {
				out <- ir.Event{
					Kind:         ir.EvUsage,
					InputTokens:  int(usage.Get("input_tokens").Int()),
					OutputTokens: int(usage.Get("output_tokens").Int()),
				}
			}
			out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
			return
		}
	}

	// If we exit the loop without a done event, send done anyway.
	out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
}

func (p *Provider) probeReal(ctx context.Context, acc *domain.Account) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := p.getToken(ctx, acc.ID)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexBaseURL+"/models", nil)
	if err != nil {
		return err
	}
	setAuthHeaders(req, token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cursor: probe failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("cursor: probe HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (p *Provider) discoverReal(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := p.getToken(ctx, acc.ID)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexBaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	setAuthHeaders(req, token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: discover failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("cursor: read discover body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cursor: discover HTTP %d: %s", resp.StatusCode, string(body))
	}

	var models []domain.ModelCapability
	// Try parsing as {"models": [...]} or as top-level array
	modelsJSON := gjson.GetBytes(body, "models")
	if !modelsJSON.Exists() {
		modelsJSON = gjson.ParseBytes(body)
	}

	modelsJSON.ForEach(func(_, v gjson.Result) bool {
		id := v.Get("id").String()
		if id == "" {
			id = v.String()
		}
		if id != "" {
			models = append(models, domain.ModelCapability{
				ID:             id,
				SupportsTools:  true,
				SupportsVision: true,
			})
		}
		return true
	})

	return &domain.QuotaState{
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
}

// ---------------------------------------------------------------------------
// mock mode
// ---------------------------------------------------------------------------

func (p *Provider) discoverMock() (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "claude-sonnet-4-6", SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-7", SupportsTools: true, SupportsVision: true},
		{ID: "gpt-4o", SupportsTools: true, SupportsVision: true},
		{ID: "cursor-small", SupportsTools: false, SupportsVision: false},
	}
	return &domain.QuotaState{
		ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
		LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
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
			"[mock cursor provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from cursor provider.",
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
