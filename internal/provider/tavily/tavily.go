package tavily

import (
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

const searchURL = "https://api.tavily.com/search"

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
		httpClient: transport.ForProvider("api.tavily.com", transport.Options{Timeout: 120 * time.Second}),
	}
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func (p *Provider) Name() string { return "tavily" }

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
		return fmt.Errorf("tavily probe: %w", err)
	}
	body, _ := json.Marshal(map[string]any{
		"api_key": sec.SessionToken,
		"query":   "test",
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, searchURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(r)
	if err != nil {
		return fmt.Errorf("tavily probe: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("tavily probe: status %d", resp.StatusCode)
	}
	return nil
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "tavily-search", SupportsTools: false, SupportsVision: false},
		{ID: "tavily-extract", SupportsTools: false, SupportsVision: false},
	}
	return &domain.QuotaState{
		ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
		LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}

	// Extract last user message as the search query
	var query string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == ir.RoleUser {
			for _, pp := range req.Messages[i].Parts {
				if pp.Kind == ir.PartText {
					query = pp.Text
					break
				}
			}
			break
		}
	}
	if query == "" {
		return nil, fmt.Errorf("tavily: no user message found for search query")
	}

	body, err := json.Marshal(map[string]any{
		"api_key": sec.SessionToken,
		"query":   query,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, searchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tavily invoke: %w", err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("tavily invoke: status %d: %s", resp.StatusCode, string(b))
	}

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("tavily invoke: %w", err)
	}

	// Format search results as text
	var sb strings.Builder
	answer := gjson.GetBytes(respBody, "answer").String()
	if answer != "" {
		sb.WriteString("Answer: ")
		sb.WriteString(answer)
		sb.WriteString("\n\n")
	}
	results := gjson.GetBytes(respBody, "results")
	if results.IsArray() {
		results.ForEach(func(_, v gjson.Result) bool {
			title := v.Get("title").String()
			url := v.Get("url").String()
			content := v.Get("content").String()
			sb.WriteString(fmt.Sprintf("### %s\n%s\n%s\n\n", title, url, content))
			return true
		})
	}
	text := sb.String()
	if text == "" {
		text = "No search results found."
	}

	out := make(chan ir.Event, 4)
	go func() {
		defer close(out)
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		case out <- ir.Event{Kind: ir.EvTextDelta, Text: text}:
		}
		out <- ir.Event{Kind: ir.EvUsage, InputTokens: len(query), OutputTokens: len(text)}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
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
			"[mock tavily provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from tavily provider.",
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
