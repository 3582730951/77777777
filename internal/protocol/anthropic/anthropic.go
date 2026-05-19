// Package anthropic implements Anthropic Messages API decode/encode.
package anthropic

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

type request struct {
	Model             string          `json:"model"`
	System            json.RawMessage `json:"system,omitempty"`
	Messages          []rawMessage    `json:"messages"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxTokens         int             `json:"max_tokens"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []rawTool       `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	Thinking          *thinkingCfg    `json:"thinking,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
}

type thinkingCfg struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type rawTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

func Decode(r io.Reader) (*ir.Request, error) {
	var req request
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode anthropic request: %w", err)
	}
	// Parse "model effort" combined syntax: "/model claude-opus-4-7 max"
	// sends model="claude-opus-4-7 max". Split effort from model name.
	modelName, effortStr := splitClaudeModelEffort(req.Model)

	out := &ir.Request{
		Model:           modelName,
		Stream:          req.Stream,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		ReasoningEffort: effortStr,
		OriginalModel:   modelName,
		OriginalProto:   "anthropic",
	}
	if req.Thinking != nil {
		out.ThinkingTokens = req.Thinking.BudgetTokens
		out.ThinkingType = req.Thinking.Type
	} else if effortStr != "" && effortStr != "none" {
		// Auto-enable thinking when effort is specified via /model syntax.
		out.ThinkingTokens = effortToClaudeBudget(effortStr)
		out.ThinkingType = "adaptive"
	}
	if len(req.System) > 0 {
		out.System = decodeText(req.System)
		out.AnthropicSystemText = out.System
		out.AnthropicSystem = cloneRaw(req.System)
	}
	if len(req.Metadata) > 0 {
		out.AnthropicMetadata = cloneRaw(req.Metadata)
	}
	if len(req.ContextManagement) > 0 {
		out.AnthropicContextManagement = cloneRaw(req.ContextManagement)
	}
	if len(req.ToolChoice) > 0 {
		out.AnthropicToolChoice = cloneRaw(req.ToolChoice)
		parseToolChoice(req.ToolChoice, &out.ToolChoice)
	}
	for _, m := range req.Messages {
		msg := ir.Message{Role: ir.Role(m.Role)}
		var arr []map[string]any
		if json.Unmarshal(m.Content, &arr) == nil {
			for _, blk := range arr {
				if t, _ := blk["type"].(string); t == "text" {
					if txt, _ := blk["text"].(string); txt != "" {
						msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: txt, CacheControl: rawBlockField(blk, "cache_control")})
					}
				} else if t == "image" {
					src, _ := blk["source"].(map[string]any)
					if src != nil {
						sourceType, _ := src["type"].(string)
						media, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						url, _ := src["url"].(string)
						part := ir.Part{Kind: ir.PartImage, ImageMedia: media, CacheControl: rawBlockField(blk, "cache_control")}
						if sourceType == "url" {
							part.ImageURL = url
						} else {
							part.ImageBytes = []byte(data)
						}
						msg.Parts = append(msg.Parts, part)
					}
				} else if t == "tool_use" {
					id, _ := blk["id"].(string)
					name, _ := blk["name"].(string)
					inb, _ := json.Marshal(blk["input"])
					msg.Parts = append(msg.Parts, ir.Part{
						Kind: ir.PartToolUse, ToolUseID: id, ToolUseName: name, ToolUseInput: inb, CacheControl: rawBlockField(blk, "cache_control"),
					})
				} else if t == "tool_result" {
					id, _ := blk["tool_use_id"].(string)
					var content []byte
					if c, ok := blk["content"]; ok {
						content, _ = json.Marshal(c)
					}
					isErr, _ := blk["is_error"].(bool)
					msg.Parts = append(msg.Parts, ir.Part{
						Kind: ir.PartToolResult, ToolResultID: id, ToolResultBytes: content, ToolResultErr: isErr, CacheControl: rawBlockField(blk, "cache_control"),
					})
				}
			}
		} else {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: s})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ir.ToolDef{
			Name: t.Name, Description: t.Description, Schema: cloneRaw(t.InputSchema), CacheControl: cloneRaw(t.CacheControl),
		})
	}
	return out, nil
}

func cloneRaw(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func rawBlockField(block map[string]any, key string) []byte {
	v, ok := block[key]
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func parseToolChoice(raw json.RawMessage, out *ir.ToolChoice) {
	if out == nil || len(raw) == 0 {
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		out.Mode = s
		return
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	out.Mode = obj.Type
	out.Name = obj.Name
}

func decodeText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		var out string
		for _, b := range arr {
			if t, _ := b["type"].(string); t == "text" {
				if txt, _ := b["text"].(string); txt != "" {
					if out != "" {
						out += "\n"
					}
					out += txt
				}
			}
		}
		return out
	}
	return ""
}

type Encoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	stream  bool
	msgID   string

	contentIdx int
	textOpen   bool
	toolOpen   bool
	thinkOpen  bool
	curToolID  string
}

func (e *Encoder) closeCurrentBlock() {
	if e.textOpen {
		_ = e.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.contentIdx})
		e.contentIdx++
		e.textOpen = false
	}
	if e.toolOpen {
		_ = e.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.contentIdx})
		e.contentIdx++
		e.toolOpen = false
	}
	if e.thinkOpen {
		_ = e.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.contentIdx})
		e.contentIdx++
		e.thinkOpen = false
	}
}

func NewEncoder(w http.ResponseWriter, displayModel string, stream bool) *Encoder {
	flusher, _ := w.(http.Flusher)
	return &Encoder{
		w:       w,
		flusher: flusher,
		model:   displayModel,
		stream:  stream,
		msgID:   "msg_" + randID(),
	}
}

func (e *Encoder) WriteHeaders() {
	if e.stream {
		e.w.Header().Set("Content-Type", "text/event-stream")
		e.w.Header().Set("Cache-Control", "no-cache, no-transform")
		e.w.Header().Set("Connection", "keep-alive")
		e.w.Header().Set("X-Accel-Buffering", "no")
	} else {
		e.w.Header().Set("Content-Type", "application/json")
	}
	e.w.WriteHeader(http.StatusOK)
}

func (e *Encoder) writeEvent(name string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, b)
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

func (e *Encoder) Stream(events <-chan ir.Event) error {
	if !e.stream {
		return e.collect(events)
	}
	if err := e.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          e.msgID,
			"type":        "message",
			"role":        "assistant",
			"model":       e.model,
			"content":     []any{},
			"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
			"stop_reason": nil,
		},
	}); err != nil {
		return err
	}
	for ev := range events {
		switch ev.Kind {
		case ir.EvThinkingDelta:
			if !e.thinkOpen {
				e.closeCurrentBlock()
				if err := e.writeEvent("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         e.contentIdx,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				}); err != nil {
					return err
				}
				e.thinkOpen = true
			}
			if err := e.writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.contentIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
			}); err != nil {
				return err
			}
		case ir.EvTextDelta:
			if !e.textOpen {
				e.closeCurrentBlock()
				if err := e.writeEvent("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         e.contentIdx,
					"content_block": map[string]any{"type": "text", "text": ""},
				}); err != nil {
					return err
				}
				e.textOpen = true
			}
			if err := e.writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.contentIdx,
				"delta": map[string]any{"type": "text_delta", "text": ev.Text},
			}); err != nil {
				return err
			}
		case ir.EvToolUseStart:
			e.closeCurrentBlock()
			e.toolOpen = true
			e.curToolID = ev.ToolID
			if err := e.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": e.contentIdx,
				"content_block": map[string]any{
					"type": "tool_use", "id": ev.ToolID, "name": ev.ToolName, "input": map[string]any{},
				},
			}); err != nil {
				return err
			}
		case ir.EvToolUseDelta:
			if err := e.writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.contentIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(ev.ToolDelta)},
			}); err != nil {
				return err
			}
		case ir.EvDone:
			e.closeCurrentBlock()
			reason := ev.FinishReason
			if reason == "" {
				reason = "end_turn"
			}
			_ = e.writeEvent("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
				"usage": map[string]int{"output_tokens": ev.OutputTokens},
			})
			_ = e.writeEvent("message_stop", map[string]any{"type": "message_stop"})
			return nil
		case ir.EvError:
			return ev.Err
		}
	}
	return nil
}

type response struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []respBlock    `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      map[string]int `json:"usage"`
}

type respBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

func (e *Encoder) collect(events <-chan ir.Event) error {
	resp := response{
		ID: e.msgID, Type: "message", Role: "assistant", Model: e.model, StopReason: "end_turn",
		Usage: map[string]int{"input_tokens": 0, "output_tokens": 0}, // never nil
	}
	var curText string
	var curToolJSON string
	curToolIdx := -1
	flushTool := func() {
		if curToolIdx >= 0 && curToolJSON != "" {
			var input map[string]any
			_ = json.Unmarshal([]byte(curToolJSON), &input)
			resp.Content[curToolIdx].Input = input
		}
	}
	for ev := range events {
		switch ev.Kind {
		case ir.EvTextDelta:
			curText += ev.Text
		case ir.EvToolUseStart:
			flushTool()
			if curText != "" {
				resp.Content = append(resp.Content, respBlock{Type: "text", Text: curText})
				curText = ""
			}
			resp.Content = append(resp.Content, respBlock{Type: "tool_use", ID: ev.ToolID, Name: ev.ToolName})
			curToolIdx = len(resp.Content) - 1
			curToolJSON = ""
		case ir.EvToolUseDelta:
			curToolJSON += string(ev.ToolDelta)
		case ir.EvUsage:
			resp.Usage = map[string]int{"input_tokens": ev.InputTokens, "output_tokens": ev.OutputTokens}
		case ir.EvDone:
			if ev.FinishReason != "" {
				resp.StopReason = ev.FinishReason
			}
		case ir.EvError:
			return ev.Err
		}
	}
	if curText != "" {
		resp.Content = append(resp.Content, respBlock{Type: "text", Text: curText})
	}
	flushTool()
	return json.NewEncoder(e.w).Encode(resp)
}

func randID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 24)
	var b [24]byte
	_, _ = rand.Read(b[:])
	for i := range out {
		out[i] = charset[int(b[i])%len(charset)]
	}
	return string(out)
}

// splitClaudeModelEffort parses "claude-opus-4-7 max" → ("claude-opus-4-7", "max").
// Valid Claude effort tokens: none | low | medium | high | max
func splitClaudeModelEffort(raw string) (model, effort string) {
	known := map[string]bool{
		"none": true, "low": true, "medium": true,
		"high": true, "max": true, "maximum": true, "auto": true,
	}
	idx := strings.LastIndex(raw, " ")
	if idx < 0 {
		return raw, ""
	}
	last := strings.ToLower(strings.TrimSpace(raw[idx+1:]))
	if known[last] {
		return strings.TrimSpace(raw[:idx]), last
	}
	return raw, ""
}

// effortToClaudeBudget maps named effort → Claude thinking budget_tokens.
func effortToClaudeBudget(effort string) int {
	switch strings.ToLower(effort) {
	case "none", "0":
		return 0
	case "low", "minimal":
		return 1024
	case "medium", "normal":
		return 8000
	case "high":
		return 16000
	case "max", "maximum":
		return 32000
	}
	return 8000
}
