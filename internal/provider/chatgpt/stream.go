// stream.go — parses ChatGPT's /backend-api/conversation SSE stream into IR
// events.
//
// The stream uses a JSON Patch protocol. Two event shapes:
//
//	data: {"v": {"message": {"content": {"parts": ["initial text"]}, ...}}}
//	     — full or partial message snapshot. We use this to seed the buffer.
//
//	data: {"p":"/message/content/parts/0", "o":"append", "v":" more text"}
//	     — incremental patch. We track the accumulated text per message.
//
//	data: [DONE]
//	     — stream terminator.
//
// We only care about content-text deltas for now; tool calls and image blocks
// are decoded best-effort. Anything we don't understand is silently ignored,
// keeping the stream robust against minor protocol drift.
package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func streamSSE(ctx context.Context, body io.ReadCloser, out chan<- ir.Event, _ string) {
	defer close(out)
	defer body.Close()

	br := bufio.NewReaderSize(body, 16*1024)
	var (
		acc           strings.Builder // accumulated text for parts[0]
		emittedSoFar  int             // bytes already emitted as TextDelta
		usageInput    int
		usageOutput   int
		finishReason  string
		messageStatus string
	)

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
				flushDelta(&acc, &emittedSoFar, out)
				out <- ir.Event{Kind: ir.EvUsage, InputTokens: usageInput, OutputTokens: usageOutput}
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
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			flushDelta(&acc, &emittedSoFar, out)
			if finishReason == "" {
				finishReason = "stop"
			}
			out <- ir.Event{Kind: ir.EvUsage, InputTokens: usageInput, OutputTokens: usageOutput}
			out <- ir.Event{Kind: ir.EvDone, FinishReason: finishReason}
			return
		}
		// Try to parse as either snapshot (object with `v`) or patch.
		applyEvent([]byte(data), &acc, &emittedSoFar, &finishReason, &messageStatus, &usageInput, &usageOutput, out)
	}
}

func applyEvent(b []byte, acc *strings.Builder, emitted *int, finish, status *string, usageIn, usageOut *int, out chan<- ir.Event) {
	// Try as either:
	//   snapshot: {"v": {...}}
	//   patch list: [{"p":"...","o":"...","v":...}, ...]
	//   single patch: {"p":"...","o":"...","v":...}
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "[") {
		var patches []patchOp
		if err := json.Unmarshal(b, &patches); err == nil {
			for _, p := range patches {
				applyPatch(p, acc, emitted, finish, status, usageIn, usageOut, out)
			}
			return
		}
	}
	// Determine whether it's a patch (has "p" and "o") or a snapshot (has "v" and not "p").
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return
	}
	if _, hasP := probe["p"]; hasP {
		var p patchOp
		if err := json.Unmarshal(b, &p); err == nil {
			applyPatch(p, acc, emitted, finish, status, usageIn, usageOut, out)
		}
		return
	}
	// Snapshot: {"v": {...}}
	if vRaw, ok := probe["v"]; ok {
		applySnapshot(vRaw, acc, emitted, finish, status, usageIn, usageOut, out)
	}
}

type patchOp struct {
	P string          `json:"p"`
	O string          `json:"o"`
	V json.RawMessage `json:"v"`
}

func applyPatch(p patchOp, acc *strings.Builder, emitted *int, finish, status *string, usageIn, usageOut *int, out chan<- ir.Event) {
	switch {
	case p.P == "" && p.O == "":
		// no-op
	case strings.HasPrefix(p.P, "/message/content/parts/") && p.O == "append":
		var s string
		if err := json.Unmarshal(p.V, &s); err == nil {
			acc.WriteString(s)
			emit(acc, emitted, out)
		}
	case strings.HasPrefix(p.P, "/message/content/parts/0") && p.O == "replace":
		var s string
		if err := json.Unmarshal(p.V, &s); err == nil {
			acc.Reset()
			acc.WriteString(s)
			*emitted = 0
			emit(acc, emitted, out)
		}
	case p.P == "/message/status" && p.O == "replace":
		var s string
		_ = json.Unmarshal(p.V, &s)
		*status = s
		if s == "finished_successfully" && *finish == "" {
			*finish = "stop"
		}
	case p.P == "/message/end_turn" && p.O == "replace":
		// boolean true: end of turn
	case strings.Contains(p.P, "metadata/finish_details") && p.O == "replace":
		var fd struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p.V, &fd)
		if fd.Type != "" {
			*finish = fd.Type
		}
	case strings.Contains(p.P, "/usage/input_tokens"):
		var n int
		_ = json.Unmarshal(p.V, &n)
		*usageIn = n
	case strings.Contains(p.P, "/usage/output_tokens"):
		var n int
		_ = json.Unmarshal(p.V, &n)
		*usageOut = n
	}
}

func applySnapshot(vRaw json.RawMessage, acc *strings.Builder, emitted *int, finish, status *string, usageIn, usageOut *int, out chan<- ir.Event) {
	// Snapshot might be a string, an array, or the wider message object.
	trim := strings.TrimSpace(string(vRaw))
	if strings.HasPrefix(trim, `"`) {
		var s string
		if err := json.Unmarshal(vRaw, &s); err == nil {
			acc.WriteString(s)
			emit(acc, emitted, out)
		}
		return
	}
	if strings.HasPrefix(trim, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal(vRaw, &arr); err == nil {
			for _, e := range arr {
				var p patchOp
				if err := json.Unmarshal(e, &p); err == nil {
					applyPatch(p, acc, emitted, finish, status, usageIn, usageOut, out)
				}
			}
		}
		return
	}
	var msg struct {
		Message struct {
			Content struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			Status   string                 `json:"status"`
			EndTurn  *bool                  `json:"end_turn"`
			Metadata map[string]interface{} `json:"metadata"`
		} `json:"message"`
	}
	if err := json.Unmarshal(vRaw, &msg); err != nil {
		return
	}
	if len(msg.Message.Content.Parts) > 0 {
		var s string
		if err := json.Unmarshal(msg.Message.Content.Parts[0], &s); err == nil {
			acc.Reset()
			acc.WriteString(s)
			*emitted = 0
			emit(acc, emitted, out)
		}
	}
	if msg.Message.Status == "finished_successfully" && *finish == "" {
		*finish = "stop"
	}
}

func emit(acc *strings.Builder, emittedSoFar *int, out chan<- ir.Event) {
	s := acc.String()
	if len(s) <= *emittedSoFar {
		return
	}
	if err := switchMessageError(s); err != nil {
		out <- ir.Event{Kind: ir.EvError, Err: err}
		return
	}
	delta := s[*emittedSoFar:]
	*emittedSoFar = len(s)
	out <- ir.Event{Kind: ir.EvTextDelta, Text: delta}
}

func flushDelta(acc *strings.Builder, emittedSoFar *int, out chan<- ir.Event) {
	emit(acc, emittedSoFar, out)
}

var errStreamClosed = errors.New("stream closed")
