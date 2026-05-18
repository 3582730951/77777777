// Package openai implements OpenAI Chat Completions wire protocol decode/encode.
// Decode parses an inbound /v1/chat/completions request body into IR.Request.
// Encode consumes an IR event channel and writes Chat Completions-style SSE
// chunks (or non-streaming JSON) to the provided ResponseWriter.
package openai

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

type chatRequest struct {
	Model       string      `json:"model"`
	Messages    []chatMsg   `json:"messages"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Stream      bool        `json:"stream,omitempty"`
	ServiceTier string      `json:"service_tier,omitempty"`
	Tools       []chatTool  `json:"tools,omitempty"`
	ToolChoice  any         `json:"tool_choice,omitempty"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// Decode reads an OpenAI Chat Completions request body and produces IR.Request.
// Uses gjson for zero-allocation field extraction on the hot path.
func Decode(r io.Reader) (*ir.Request, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read openai request: %w", err)
	}
	return DecodeBytes(body)
}

// DecodeBytes parses an already-read request body. Exported for compact.go reuse.
func DecodeBytes(body []byte) (*ir.Request, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("decode openai request: invalid JSON")
	}
	model   := gjson.GetBytes(body, "model").String()
	stream  := gjson.GetBytes(body, "stream").Bool()
	maxTok  := int(gjson.GetBytes(body, "max_tokens").Int())
	reason  := gjson.GetBytes(body, "reasoning_effort").String()
	serviceTier := gjson.GetBytes(body, "service_tier").String()
	var temp, topP *float64
	if t := gjson.GetBytes(body, "temperature"); t.Exists() {
		v := t.Float(); temp = &v
	}
	if t := gjson.GetBytes(body, "top_p"); t.Exists() {
		v := t.Float(); topP = &v
	}

	// Parse "model effort" combined syntax from /model command:
	// "/model gpt-5.5 high" sends model="gpt-5.5 high" — split on last space.
	if reason == "" {
		model, reason = splitModelEffort(model)
	}

	out := &ir.Request{
		Model:           model,
		Stream:          stream,
		MaxTokens:       maxTok,
		Temperature:     temp,
		TopP:            topP,
		ServiceTier:     serviceTier,
		ReasoningEffort: reason,
		OriginalModel:   model,
		OriginalProto:   "openai",
	}

	// Detect Responses API format (has "input" array instead of "messages")
	if gjson.GetBytes(body, "input").Exists() && !gjson.GetBytes(body, "messages").Exists() {
		decodeResponsesInput(body, out)
		// Responses API uses "instructions" instead of system message
		if inst := gjson.GetBytes(body, "instructions").String(); inst != "" {
			out.System = inst
		}
		// Responses API reasoning
		if eff := gjson.GetBytes(body, "reasoning.effort").String(); eff != "" {
			out.ReasoningEffort = eff
		}
		// Responses API always streams
		out.Stream = true
		// Tools
		gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
			if t.Get("type").String() == "function" {
				out.Tools = append(out.Tools, ir.ToolDef{
					Name:        t.Get("name").String(),
					Description: t.Get("description").String(),
					Schema:      []byte(t.Get("parameters").Raw),
				})
			}
			return true
		})
		return out, nil
	}

	// Messages (Chat Completions format)
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		if role == "system" || role == "developer" {
			text := gjsonContent(m.Get("content"))
			out.System = appendText(out.System, text)
			return true
		}
		msg := ir.Message{Role: ir.Role(role)}
		content := m.Get("content")
		if content.Type == gjson.String {
			msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: content.String()})
		} else if content.IsArray() {
			content.ForEach(func(_, blk gjson.Result) bool {
				switch blk.Get("type").String() {
				case "text":
					if t := blk.Get("text").String(); t != "" {
						msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: t})
					}
				case "image_url":
					url := blk.Get("image_url.url").String()
					media, data := parseDataURI(url)
					if data != "" {
						msg.Parts = append(msg.Parts, ir.Part{
							Kind: ir.PartImage, ImageMedia: media, ImageBytes: []byte(data),
						})
					} else if url != "" {
						msg.Parts = append(msg.Parts, ir.Part{
							Kind: ir.PartImage, ImageURL: url,
						})
					}
				}
				return true
			})
		}
		// tool_calls array
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			msg.Parts = append(msg.Parts, ir.Part{
				Kind:         ir.PartToolUse,
				ToolUseID:    tc.Get("id").String(),
				ToolUseName:  tc.Get("function.name").String(),
				ToolUseInput: []byte(tc.Get("function.arguments").String()),
			})
			return true
		})
		// tool result
		if tid := m.Get("tool_call_id").String(); tid != "" {
			msg.Parts = append(msg.Parts, ir.Part{
				Kind:            ir.PartToolResult,
				ToolResultID:    tid,
				ToolResultBytes: []byte(gjsonContent(m.Get("content"))),
			})
		}
		out.Messages = append(out.Messages, msg)
		return true
	})

	// Tools
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		out.Tools = append(out.Tools, ir.ToolDef{
			Name:        t.Get("function.name").String(),
			Description: t.Get("function.description").String(),
			Schema:      []byte(t.Get("function.parameters").Raw),
		})
		return true
	})

	// tool_choice
	tc := gjson.GetBytes(body, "tool_choice")
	if tc.Type == gjson.String {
		out.ToolChoice.Mode = tc.String()
	} else if tc.IsObject() {
		out.ToolChoice.Mode = "function"
		out.ToolChoice.Name = tc.Get("function.name").String()
	}

	return out, nil
}

// gjsonContent extracts text content from either a string or content-block array.
func gjsonContent(r gjson.Result) string {
	if !r.Exists() {
		return ""
	}
	if r.Type == gjson.String {
		return r.String()
	}
	if r.IsArray() {
		var b strings.Builder
		r.ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "text" {
				if t := blk.Get("text").String(); t != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(t)
				}
			}
			return true
		})
		return b.String()
	}
	return ""
}

// decodeResponsesInput parses the Responses API "input" array into IR messages.
// Responses API format:
//   input: [
//     {"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]},
//     {"type":"function_call","name":"...","call_id":"...","arguments":"..."},
//     {"type":"function_call_output","call_id":"...","output":"..."},
//   ]
func decodeResponsesInput(body []byte, out *ir.Request) {
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		switch itemType {
		case "message":
			role := item.Get("role").String()
			if role == "system" || role == "developer" {
				// system instructions
				item.Get("content").ForEach(func(_, c gjson.Result) bool {
					if c.Get("type").String() == "input_text" {
						out.System = appendText(out.System, c.Get("text").String())
					}
					return true
				})
				return true
			}
			msg := ir.Message{Role: ir.Role(role)}
			content := item.Get("content")
			if content.Type == gjson.String {
				msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: content.String()})
			} else if content.IsArray() {
				content.ForEach(func(_, c gjson.Result) bool {
					switch c.Get("type").String() {
					case "input_text":
						msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: c.Get("text").String()})
					case "output_text":
						msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: c.Get("text").String()})
					case "input_image":
						url := c.Get("image_url").String()
						media, data := parseDataURI(url)
						if data != "" {
							msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartImage, ImageMedia: media, ImageBytes: []byte(data)})
						} else if url != "" {
							msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartImage, ImageURL: url})
						}
					}
					return true
				})
			}
			if len(msg.Parts) > 0 {
				out.Messages = append(out.Messages, msg)
			}
		case "function_call":
			msg := ir.Message{Role: ir.RoleAssistant}
			msg.Parts = append(msg.Parts, ir.Part{
				Kind:         ir.PartToolUse,
				ToolUseID:    item.Get("call_id").String(),
				ToolUseName:  item.Get("name").String(),
				ToolUseInput: []byte(item.Get("arguments").String()),
			})
			out.Messages = append(out.Messages, msg)
		case "function_call_output":
			msg := ir.Message{Role: ir.RoleTool}
			msg.Parts = append(msg.Parts, ir.Part{
				Kind:            ir.PartToolResult,
				ToolResultID:    item.Get("call_id").String(),
				ToolResultBytes: []byte(item.Get("output").String()),
			})
			out.Messages = append(out.Messages, msg)
		}
		return true
	})
}

// parseDataURI extracts media type and base64 data from a data URI.
// Returns ("","") if not a data URI.
func parseDataURI(url string) (mediaType, data string) {
	if !strings.HasPrefix(url, "data:") {
		return "", ""
	}
	// data:image/png;base64,iVBOR...
	rest := url[5:]
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", ""
	}
	return rest[:semi], rest[semi+8:]
}

func decodeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		var b strings.Builder
		for _, item := range arr {
			if t, ok := item["type"].(string); ok && t == "text" {
				if txt, ok := item["text"].(string); ok {
					b.WriteString(txt)
				}
			}
		}
		return b.String()
	}
	return ""
}

func appendText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}

// Encoder writes IR events to an HTTP response in OpenAI Chat Completions format.
type Encoder struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	model       string
	id          string
	created     int64
	stream      bool
	finishSent  bool
	toolBufID   string
	toolBufName string
}

func NewEncoder(w http.ResponseWriter, displayModel string, stream bool) *Encoder {
	flusher, _ := w.(http.Flusher)
	return &Encoder{
		w:       w,
		flusher: flusher,
		model:   displayModel,
		id:      "chatcmpl-" + randID(),
		created: time.Now().Unix(),
		stream:  stream,
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

type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
}

type streamChoice struct {
	Index        int       `json:"index"`
	Delta        delta     `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type delta struct {
	Role             string         `json:"role,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

func (e *Encoder) Stream(events <-chan ir.Event) error {
	if !e.stream {
		return e.collectAndEmit(events)
	}
	first := true
	for ev := range events {
		switch ev.Kind {
		case ir.EvTextDelta:
			ch := streamChunk{
				ID: e.id, Object: "chat.completion.chunk",
				Created: e.created, Model: e.model,
				Choices: []streamChoice{{Index: 0, Delta: delta{}}},
			}
			if first {
				ch.Choices[0].Delta.Role = "assistant"
				first = false
			}
			ch.Choices[0].Delta.Content = ev.Text
			if err := e.writeSSE(ch); err != nil {
				return err
			}
		case ir.EvToolUseStart:
			e.toolBufID, e.toolBufName = ev.ToolID, ev.ToolName
			ch := streamChunk{
				ID: e.id, Object: "chat.completion.chunk",
				Created: e.created, Model: e.model,
				Choices: []streamChoice{{
					Index: 0, Delta: delta{
						ToolCalls: []chatToolCall{{ID: ev.ToolID, Type: "function"}},
					},
				}},
			}
			ch.Choices[0].Delta.ToolCalls[0].Function.Name = ev.ToolName
			if err := e.writeSSE(ch); err != nil {
				return err
			}
		case ir.EvToolUseDelta:
			ch := streamChunk{
				ID: e.id, Object: "chat.completion.chunk",
				Created: e.created, Model: e.model,
				Choices: []streamChoice{{
					Index: 0, Delta: delta{
						ToolCalls: []chatToolCall{{ID: e.toolBufID, Type: "function"}},
					},
				}},
			}
			ch.Choices[0].Delta.ToolCalls[0].Function.Arguments = string(ev.ToolDelta)
			if err := e.writeSSE(ch); err != nil {
				return err
			}
		case ir.EvThinkingDelta:
			ch := streamChunk{
				ID: e.id, Object: "chat.completion.chunk",
				Created: e.created, Model: e.model,
				Choices: []streamChoice{{Index: 0, Delta: delta{}}},
			}
			if first {
				ch.Choices[0].Delta.Role = "assistant"
				first = false
			}
			ch.Choices[0].Delta.ReasoningContent = ev.Text
			if err := e.writeSSE(ch); err != nil {
				return err
			}
		case ir.EvDone:
			reason := mapFinishReason(ev.FinishReason)
			ch := streamChunk{
				ID: e.id, Object: "chat.completion.chunk",
				Created: e.created, Model: e.model,
				Choices: []streamChoice{{Index: 0, Delta: delta{}, FinishReason: &reason}},
			}
			if err := e.writeSSE(ch); err != nil {
				return err
			}
			_, _ = e.w.Write([]byte("data: [DONE]\n\n"))
			if e.flusher != nil {
				e.flusher.Flush()
			}
			e.finishSent = true
			return nil
		case ir.EvError:
			return ev.Err
		}
	}
	if !e.finishSent {
		reason := "stop"
		ch := streamChunk{
			ID: e.id, Object: "chat.completion.chunk",
			Created: e.created, Model: e.model,
			Choices: []streamChoice{{Index: 0, Delta: delta{}, FinishReason: &reason}},
		}
		_ = e.writeSSE(ch)
		_, _ = e.w.Write([]byte("data: [DONE]\n\n"))
		if e.flusher != nil {
			e.flusher.Flush()
		}
	}
	return nil
}

var ssePrefix = []byte("data: ")
var sseSuffix = []byte("\n\n")

func (e *Encoder) writeSSE(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(ssePrefix)+len(b)+len(sseSuffix))
	buf = append(buf, ssePrefix...)
	buf = append(buf, b...)
	buf = append(buf, sseSuffix...)
	if _, err := e.w.Write(buf); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

type completion struct {
	ID      string  `json:"id"`
	Object  string  `json:"object"`
	Created int64   `json:"created"`
	Model   string  `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int      `json:"index"`
	Message      respMsg  `json:"message"`
	FinishReason string   `json:"finish_reason"`
}

type respMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (e *Encoder) collectAndEmit(events <-chan ir.Event) error {
	var sb strings.Builder
	var reasoning strings.Builder
	var u usage
	finish := "stop"
	var toolCalls []chatToolCall
	curToolIdx := -1
	for ev := range events {
		switch ev.Kind {
		case ir.EvTextDelta:
			sb.WriteString(ev.Text)
		case ir.EvThinkingDelta:
			reasoning.WriteString(ev.Text)
		case ir.EvToolUseStart:
			tc := chatToolCall{ID: ev.ToolID, Type: "function"}
			tc.Function.Name = ev.ToolName
			toolCalls = append(toolCalls, tc)
			curToolIdx = len(toolCalls) - 1
		case ir.EvToolUseDelta:
			if curToolIdx >= 0 {
				toolCalls[curToolIdx].Function.Arguments += string(ev.ToolDelta)
			}
		case ir.EvUsage:
			u.PromptTokens = ev.InputTokens
			u.CompletionTokens = ev.OutputTokens
			u.TotalTokens = ev.InputTokens + ev.OutputTokens
		case ir.EvDone:
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		case ir.EvError:
			return ev.Err
		}
	}
	resp := completion{
		ID: e.id, Object: "chat.completion",
		Created: e.created, Model: e.model,
		Choices: []choice{{
			Index: 0,
			Message: respMsg{
				Role: "assistant", Content: sb.String(),
				ReasoningContent: reasoning.String(),
				ToolCalls: toolCalls,
			},
			FinishReason: mapFinishReason(finish),
		}},
		Usage: &u,
	}
	return json.NewEncoder(e.w).Encode(resp)
}

// mapFinishReason converts Anthropic stop_reason to OpenAI finish_reason.
func mapFinishReason(reason string) string {
	switch reason {
	case "end_turn", "":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return reason
	}
}

var randSrc = struct {
	i int
}{}

func randID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 24)
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := range out {
			out[i] = charset[(now>>uint(i*3))%int64(len(charset))]
		}
		return string(out)
	}
	for i := range out {
		out[i] = charset[int(b[i])%len(charset)]
	}
	return string(out)
}

// splitModelEffort parses a combined "model effort" string that Claude Code
// and Codex CLI send when the user runs /model <name> <effort>.
// Examples: "gpt-5.5 high" → ("gpt-5.5","high")
//           "claude-opus-4-7 max" → ("claude-opus-4-7","max")
//           "gpt-5.5" → ("gpt-5.5","")
// Valid effort tokens (case-insensitive):
//   Codex: low | medium | high | xhigh
//   Claude: none | low | medium | high | max
func splitModelEffort(raw string) (model, effort string) {
	known := map[string]bool{
		"low": true, "medium": true, "high": true,
		"xhigh": true, "max": true, "maximum": true,
		"none": true, "auto": true, "minimal": true,
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
