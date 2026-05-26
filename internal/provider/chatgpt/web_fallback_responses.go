package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/protocol/openai"
	"github.com/llm-pool/gateway/internal/store"
)

func (p *Provider) doWebConversationResponsesFallback(ctx context.Context, acc *domain.Account, info sessionInfo, sec store.AccountSecret, responsesBody []byte) (io.ReadCloser, int, error) {
	req, err := openai.DecodeBytes(responsesBody)
	if err != nil {
		return nil, 0, fmt.Errorf("decode responses body for web fallback: %w", err)
	}
	body, err := buildConversationBody(req)
	if err != nil {
		return nil, 0, fmt.Errorf("build web conversation body for fallback: %w", err)
	}
	resp, err := p.doWebConversationWithSecret(ctx, acc, info, sec, body)
	if err != nil {
		return nil, 0, fmt.Errorf("post web conversation fallback: %w", err)
	}
	if resp.StatusCode != 200 {
		return resp.Body, resp.StatusCode, nil
	}
	return webConversationAsResponsesSSE(ctx, resp.Body, req.Model), httpStatusOK, nil
}

const httpStatusOK = 200

func webConversationAsResponsesSSE(ctx context.Context, body io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	events := make(chan ir.Event, 32)
	go streamSSE(ctx, body, events, model)

	go func() {
		responseID := "resp_" + strings.ReplaceAll(uuid4(), "-", "")
		itemID := "msg_" + strings.ReplaceAll(uuid4(), "-", "")
		var fullText strings.Builder
		usageIn := 0
		usageOut := 0

		write := func(event string, payload map[string]any) bool {
			if err := writeResponsesCompatSSEEvent(pw, event, payload); err != nil {
				_ = pw.CloseWithError(err)
				return false
			}
			return true
		}

		if !write("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":     responseID,
				"object": "response",
				"status": "in_progress",
				"model":  model,
			},
		}) {
			return
		}

		for {
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				return
			case ev, ok := <-events:
				if !ok {
					writeResponsesCompatCompleted(pw, responseID, itemID, model, fullText.String(), usageIn, usageOut)
					return
				}
				switch ev.Kind {
				case ir.EvTextDelta:
					if ev.Text == "" {
						continue
					}
					fullText.WriteString(ev.Text)
					if !write("response.output_text.delta", map[string]any{
						"type":          "response.output_text.delta",
						"response_id":   responseID,
						"item_id":       itemID,
						"output_index":  0,
						"content_index": 0,
						"delta":         ev.Text,
					}) {
						return
					}
				case ir.EvUsage:
					usageIn = ev.InputTokens
					usageOut = ev.OutputTokens
				case ir.EvDone:
					writeResponsesCompatCompleted(pw, responseID, itemID, model, fullText.String(), usageIn, usageOut)
					return
				case ir.EvError:
					msg := "upstream web conversation fallback error"
					if ev.Err != nil {
						msg = ev.Err.Error()
					}
					_ = write("response.error", map[string]any{
						"type": "response.error",
						"error": map[string]any{
							"message": msg,
							"type":    "upstream_error",
						},
					})
					_ = pw.Close()
					return
				}
			}
		}
	}()
	return pr
}

func writeResponsesCompatCompleted(w *io.PipeWriter, responseID, itemID, model, text string, inputTokens, outputTokens int) {
	item := map[string]any{
		"id":     itemID,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{
				"type": "output_text",
				"text": text,
			},
		},
	}
	if err := writeResponsesCompatSSEEvent(w, "response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"text":          text,
	}); err != nil {
		_ = w.CloseWithError(err)
		return
	}
	if err := writeResponsesCompatSSEEvent(w, "response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"response_id":  responseID,
		"output_index": 0,
		"item":         item,
	}); err != nil {
		_ = w.CloseWithError(err)
		return
	}
	if err := writeResponsesCompatSSEEvent(w, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "completed",
			"model":  model,
			"output": []any{item},
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": 0,
				},
			},
		},
	}); err != nil {
		_ = w.CloseWithError(err)
		return
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		_ = w.CloseWithError(err)
		return
	}
	_ = w.Close()
}

func writeResponsesCompatSSEEvent(w io.Writer, event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
