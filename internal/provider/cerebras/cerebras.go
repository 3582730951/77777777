package cerebras

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const cerebrasBaseURL = "https://api.cerebras.ai"

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
		httpClient: transport.ForProvider("api.cerebras.ai", transport.Options{Timeout: 120 * time.Second}),
	}
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func (p *Provider) Name() string { return "cerebras" }

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
		models := []domain.ModelCapability{
			{ID: "llama-4-scout-17b-16e", SupportsTools: true, SupportsVision: false},
			{ID: "llama3.1-8b", SupportsTools: false, SupportsVision: false},
			{ID: "llama3.1-70b", SupportsTools: true, SupportsVision: false},
		}
		return &domain.QuotaState{
			ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
			LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
			DiscoveredModels: models,
			LastDiscoveryAt:  time.Now(),
		}, nil
	}
	return p.discoverReal(ctx, acc)
}

// ── real mode helpers ──

func (p *Provider) apiKey(ctx context.Context, acc *domain.Account) (string, error) {
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return "", fmt.Errorf("cerebras: get secret: %w", err)
	}
	if sec.SessionToken == "" {
		return "", errors.New("cerebras: empty API key in SessionToken")
	}
	return sec.SessionToken, nil
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	apiKey, err := p.apiKey(ctx, acc)
	if err != nil {
		return nil, err
	}

	// Build messages array.
	var msgs []map[string]string
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		var sb strings.Builder
		for _, pt := range m.Parts {
			if pt.Kind == ir.PartText {
				sb.WriteString(pt.Text)
			}
		}
		msgs = append(msgs, map[string]string{"role": string(m.Role), "content": sb.String()})
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cerebras: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cerebrasBaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cerebras: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("cerebras: HTTP %d", resp.StatusCode)
	}

	out := make(chan ir.Event, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := line[6:]
			if data == "[DONE]" {
				break
			}
			parsed := gjson.Parse(data)

			// Check for error.
			if errMsg := parsed.Get("error.message"); errMsg.Exists() {
				out <- ir.Event{Kind: ir.EvError, Err: errors.New(errMsg.String())}
				return
			}

			// Extract text delta.
			delta := parsed.Get("choices.0.delta.content")
			if delta.Exists() && delta.String() != "" {
				out <- ir.Event{Kind: ir.EvTextDelta, Text: delta.String()}
			}

			// Extract finish reason.
			fr := parsed.Get("choices.0.finish_reason")
			if fr.Exists() && fr.String() != "" && fr.String() != "null" {
				// Check for usage in final chunk.
				usage := parsed.Get("usage")
				if usage.Exists() {
					out <- ir.Event{
						Kind:         ir.EvUsage,
						InputTokens:  int(usage.Get("prompt_tokens").Int()),
						OutputTokens: int(usage.Get("completion_tokens").Int()),
					}
				}
				out <- ir.Event{Kind: ir.EvDone, FinishReason: fr.String()}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			out <- ir.Event{Kind: ir.EvError, Err: fmt.Errorf("cerebras: stream read: %w", err)}
			return
		}
		// If we exited the loop via [DONE] without a finish_reason chunk, emit done.
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
}

func (p *Provider) probeReal(ctx context.Context, acc *domain.Account) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	apiKey, err := p.apiKey(ctx, acc)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cerebrasBaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("cerebras probe: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cerebras probe: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *Provider) discoverReal(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	apiKey, err := p.apiKey(ctx, acc)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cerebrasBaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cerebras discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cerebras discover: HTTP %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, fmt.Errorf("cerebras discover: read body: %w", err)
	}

	var models []domain.ModelCapability
	gjson.GetBytes(buf.Bytes(), "data").ForEach(func(_, v gjson.Result) bool {
		id := v.Get("id").String()
		if id == "" {
			return true
		}
		models = append(models, domain.ModelCapability{
			ID:             id,
			SupportsTools:  false,
			SupportsVision: false,
		})
		return true
	})

	return &domain.QuotaState{
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
			"[mock cerebras provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from cerebras provider.",
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
