package grok

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
	"github.com/llm-pool/gateway/internal/transport"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/tidwall/gjson"
)

const baseURL = "https://api.x.ai/v1/chat/completions"

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
		httpClient: transport.ForProvider("api.x.ai", transport.Options{Timeout: 120 * time.Second}),
	}
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func (p *Provider) Name() string { return "grok" }

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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("grok probe: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.x.ai/v1/models", nil)
	if err != nil {
		return err
	}
	setAuth(r, sec)
	resp, err := p.httpClient.Do(r)
	if err != nil {
		return fmt.Errorf("grok probe: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("grok probe: status %d", resp.StatusCode)
	}
	return nil
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "grok-3", SupportsTools: true, SupportsVision: false},
		{ID: "grok-3-mini", SupportsTools: true, SupportsVision: false},
	}
	if p.Mode != "mock" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sec, err := p.store.GetAccountSecret(ctx, acc.ID)
		if err == nil {
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.x.ai/v1/models", nil)
			if err == nil {
				setAuth(r, sec)
				resp, err := p.httpClient.Do(r)
				if err == nil {
					defer resp.Body.Close()
					b, _ := io.ReadAll(resp.Body)
					result := gjson.GetBytes(b, "data")
					if result.IsArray() {
						models = models[:0]
						result.ForEach(func(_, v gjson.Result) bool {
							models = append(models, domain.ModelCapability{
								ID:            v.Get("id").String(),
								SupportsTools: true,
							})
							return true
						})
					}
				}
			}
		}
	}
	return &domain.QuotaState{
		ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
		LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
}

func setAuth(r *http.Request, sec store.AccountSecret) {
	// If token looks like a cookie value, set as Cookie header; otherwise Bearer
	if strings.Contains(sec.SessionToken, "=") && strings.Contains(sec.SessionToken, ";") {
		r.Header.Set("Cookie", sec.SessionToken)
	} else {
		r.Header.Set("Authorization", "Bearer "+sec.SessionToken)
	}
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("grok: %w", err)
	}
	body, err := buildOpenAIBody(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setAuth(httpReq, sec)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("grok invoke: %w", err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("grok invoke: status %d: %s", resp.StatusCode, string(b))
	}
	out := make(chan ir.Event, 16)
	go func() {
		defer close(out)
		parseOpenAISSE(ctx, resp.Body, out)
	}()
	return out, nil
}

func buildOpenAIBody(req *ir.Request) ([]byte, error) {
	var messages []map[string]string
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		var content string
		for _, p := range m.Parts {
			if p.Kind == ir.PartText {
				content += p.Text
			}
		}
		role := "user"
		switch m.Role {
		case ir.RoleAssistant:
			role = "assistant"
		case ir.RoleSystem:
			role = "system"
		}
		messages = append(messages, map[string]string{"role": role, "content": content})
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
	}
	return json.Marshal(body)
}

func parseOpenAISSE(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		text := gjson.Get(data, "choices.0.delta.content").String()
		if text != "" {
			select {
			case <-ctx.Done():
				out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
				return
			case out <- ir.Event{Kind: ir.EvTextDelta, Text: text}:
			}
		}
		if gjson.Get(data, "choices.0.finish_reason").String() == "stop" {
			usage := gjson.Get(data, "usage")
			if usage.Exists() {
				out <- ir.Event{Kind: ir.EvUsage,
					InputTokens:  int(usage.Get("prompt_tokens").Int()),
					OutputTokens: int(usage.Get("completion_tokens").Int()),
				}
			}
		}
	}
	out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
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
			"[mock grok provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from grok provider.",
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

var _ = errors.New
