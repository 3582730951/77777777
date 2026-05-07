// stream_responses.go — parses ChatGPT's /backend-api/codex/responses SSE
// stream (the OpenAI Responses API protocol used by Codex CLI).
//
// Events we care about:
//
//	event: response.output_text.delta
//	data: {"delta":"...","item_id":"...","output_index":0,...}
//
//	event: response.output_text.done
//	data: {"text":"final","..."}
//
//	event: response.completed
//	data: {"response":{"usage":{"input_tokens":N,"output_tokens":M,...},...}}
//
//	event: response.error  / event: error
//	data: {"error":{"message":"..."}}
//
// Tool / image / reasoning events are ignored for now (text-only MVP).
package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

// streamResponsesSSE parses chatgpt.com/backend-api/codex/responses SSE and
// emits IR events. Supports text deltas AND function_call tool-use events so
// Claude Code plan-mode / multi-agent / task orchestration works via the GPT
// backend.
func streamResponsesSSE(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer close(out)
	defer body.Close()

	br := bufio.NewReaderSize(body, 64*1024)
	var (
		emittedSoFar int
		fullText     string
		usageIn      int
		usageOut     int
		finishReason string

		// tool-call accumulation
		curCallID   string
		curCallName string
		curCallArgs strings.Builder
		inToolCall  bool
	)

	emitTool := func() {
		if !inToolCall || curCallID == "" {
			return
		}
		out <- ir.Event{Kind: ir.EvToolUseStart, ToolID: curCallID, ToolName: curCallName}
		if args := curCallArgs.String(); args != "" {
			out <- ir.Event{Kind: ir.EvToolUseDelta, ToolDelta: []byte(args)}
		}
		curCallID = ""
		curCallName = ""
		curCallArgs.Reset()
		inToolCall = false
	}

	for {
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		default:
		}
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				emitTool()
				out <- ir.Event{Kind: ir.EvUsage, InputTokens: usageIn, OutputTokens: usageOut}
				if finishReason == "" {
					finishReason = "stop"
				}
				out <- ir.Event{Kind: ir.EvDone, FinishReason: finishReason}
				return
			}
			out <- ir.Event{Kind: ir.EvError, Err: err}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event: ") || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			emitTool()
			out <- ir.Event{Kind: ir.EvUsage, InputTokens: usageIn, OutputTokens: usageOut}
			if finishReason == "" {
				finishReason = "stop"
			}
			out <- ir.Event{Kind: ir.EvDone, FinishReason: finishReason}
			return
		}

		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		switch evt.Type {

		// ── Text ─────────────────────────────────────────────────────────────
		case "response.output_text.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Delta != "" {
				fullText += d.Delta
				if isUsageLimitMessage(fullText) {
					out <- ir.Event{Kind: ir.EvError, Err: fmt.Errorf("upstream quota: %s", fullText)}
					return
				}
				out <- ir.Event{Kind: ir.EvTextDelta, Text: d.Delta}
				emittedSoFar = len(fullText)
			}

		case "response.output_text.done":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && len(d.Text) > emittedSoFar {
				if isUsageLimitMessage(d.Text) {
					out <- ir.Event{Kind: ir.EvError, Err: fmt.Errorf("upstream quota: %s", d.Text)}
					return
				}
				delta := d.Text[emittedSoFar:]
				out <- ir.Event{Kind: ir.EvTextDelta, Text: delta}
				fullText = d.Text
				emittedSoFar = len(fullText)
			}

		// ── Tool / function_call ──────────────────────────────────────────────
		// response.output_item.added carries the start of a function_call item.
		case "response.output_item.added":
			var d struct {
				Item struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Item.Type == "function_call" {
				emitTool() // flush any previous incomplete tool
				inToolCall = true
				curCallID = d.Item.CallID
				curCallName = d.Item.Name
				curCallArgs.Reset()
				finishReason = "tool_use"
			}

		// Streaming argument deltas for the current function_call.
		case "response.function_call_arguments.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Delta != "" {
				curCallArgs.WriteString(d.Delta)
			}

		// Arguments complete — emit the full tool-use event.
		case "response.function_call_arguments.done":
			var d struct {
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Arguments != "" && curCallArgs.Len() == 0 {
				curCallArgs.WriteString(d.Arguments)
			}
			emitTool()

		// function_call item finished streaming.
		case "response.output_item.done":
			var d struct {
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &d) == nil && d.Item.Type == "function_call" {
				emitTool() // safety flush if .done arrived before arguments.done
			}

		// ── Completion ────────────────────────────────────────────────────────
		case "response.completed", "response.incomplete":
			var d struct {
				Response struct {
					Usage struct {
						InputTokens        int `json:"input_tokens"`
						OutputTokens       int `json:"output_tokens"`
						InputTokensDetails struct {
							CachedTokens int `json:"cached_tokens"`
						} `json:"input_tokens_details"`
					} `json:"usage"`
					Status string `json:"status"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(data), &d) == nil {
				usageIn = d.Response.Usage.InputTokens
				usageOut = d.Response.Usage.OutputTokens
				if d.Response.Status == "completed" && finishReason == "" {
					finishReason = "stop"
				}
			}
			emitTool()
			out <- ir.Event{
				Kind:            ir.EvUsage,
				InputTokens:     usageIn,
				OutputTokens:    usageOut,
				CacheReadTokens: d.Response.Usage.InputTokensDetails.CachedTokens,
			}
			out <- ir.Event{Kind: ir.EvDone, FinishReason: finishReason}
			return

		case "response.error", "error":
			var d struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(data), &d)
			out <- ir.Event{Kind: ir.EvError, Err: fmt.Errorf("upstream error: %s (%s)", d.Error.Message, d.Error.Type)}
			return

		// ── Informational (no-op) ─────────────────────────────────────────────
		case "response.content_part.added", "response.content_part.done",
			"response.in_progress", "response.created", "response.queued":
			// ignored
		}
	}
}

// isUsageLimitMessage detects ChatGPT's quota-exhaustion messages that arrive
// as normal text in a 200 SSE stream instead of as an error event.
func isUsageLimitMessage(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "you've hit your usage limit") ||
		strings.Contains(low, "you have hit your usage limit") ||
		strings.Contains(low, "usage limit has been reached") ||
		strings.Contains(low, "insufficient_quota") ||
		strings.Contains(low, "quota exceeded") ||
		strings.Contains(low, "usage exhausted") ||
		strings.Contains(low, "selected model is at capacity") ||
		strings.Contains(low, "model is at capacity") ||
		strings.Contains(low, "please try a different model")
}
