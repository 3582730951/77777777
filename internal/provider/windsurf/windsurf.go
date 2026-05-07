package windsurf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
)

type Provider struct {
	Mode  string
	store *store.Store
	ls    *LSManager
}

func New(mode string) *Provider {
	if mode == "" {
		mode = "mock"
	}
	p := &Provider{Mode: mode}
	if mode != "mock" {
		p.ls = NewLSManager("", "", 0)
	}
	return p
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }

func (p *Provider) Name() string { return "windsurf" }

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
	return p.ls.EnsureRunning(ctx)
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "claude-sonnet-4-6", SupportsTools: true, SupportsVision: true},
		{ID: "claude-3.5-sonnet", SupportsTools: true, SupportsVision: true},
		{ID: "gpt-4o", SupportsTools: true, SupportsVision: true},
		{ID: "gpt-4.1", SupportsTools: true, SupportsVision: true},
		{ID: "deepseek-v3", SupportsTools: true, SupportsVision: false},
	}
	return &domain.QuotaState{
		ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if err := p.ls.EnsureRunning(ctx); err != nil {
		return nil, fmt.Errorf("windsurf: LS not ready: %w", err)
	}

	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("windsurf: %w", err)
	}
	apiKey := sec.SessionToken
	if apiKey == "" {
		return nil, errors.New("windsurf: no api_key in account secret")
	}

	sessionID := uuid.New().String()
	model := req.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	var msgs []chatMsg
	if req.System != "" {
		msgs = append(msgs, chatMsg{id: uuid.New().String(), source: sourceSystem, text: req.System})
	}
	for _, m := range req.Messages {
		var text string
		for _, part := range m.Parts {
			if part.Kind == ir.PartText {
				text += part.Text
			}
		}
		if text == "" {
			continue
		}
		src := sourceUser
		switch m.Role {
		case ir.RoleAssistant:
			src = sourceAssistant
		case ir.RoleSystem:
			src = sourceSystem
		}
		msgs = append(msgs, chatMsg{id: uuid.New().String(), source: src, text: text})
	}

	reqProto := buildRawGetChatMessageRequest(apiKey, sessionID, model, msgs)
	body, err := p.ls.rawGetChatMessage(ctx, reqProto)
	if err != nil {
		return nil, fmt.Errorf("windsurf invoke: %w", err)
	}

	out := make(chan ir.Event, 32)
	go p.streamResponse(ctx, body, out)
	return out, nil
}

func (p *Provider) streamResponse(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer close(out)
	defer body.Close()

	var fullText strings.Builder
	for {
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		default:
		}

		payload, err := readGRPCFrame(body)
		if err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			out <- ir.Event{Kind: ir.EvError, Err: err}
			return
		}

		text, inProgress, isError := parseRawChatResponse(payload)
		if isError {
			out <- ir.Event{Kind: ir.EvError, Err: fmt.Errorf("windsurf LS error: %s", text)}
			return
		}

		// Emit delta (text beyond what we've already sent)
		if len(text) > fullText.Len() {
			delta := text[fullText.Len():]
			fullText.Reset()
			fullText.WriteString(text)
			out <- ir.Event{Kind: ir.EvTextDelta, Text: delta}
		} else if text != "" && fullText.Len() == 0 {
			fullText.WriteString(text)
			out <- ir.Event{Kind: ir.EvTextDelta, Text: text}
		}

		if !inProgress {
			break
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
			"[mock windsurf provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from windsurf provider.",
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
