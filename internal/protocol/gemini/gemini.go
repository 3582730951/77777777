// Package gemini implements Gemini generateContent (v1beta) decode/encode.
// MVP: text-only, basic tools mapped through IR.
package gemini

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

type request struct {
	Contents          []content `json:"contents"`
	SystemInstruction *content  `json:"systemInstruction,omitempty"`
	GenerationConfig  *genCfg   `json:"generationConfig,omitempty"`
	Tools             []tool    `json:"tools,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text       string      `json:"text,omitempty"`
	Thought    bool        `json:"thought,omitempty"`
	InlineData *inlineData `json:"inlineData,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type genCfg struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
}

type tool struct {
	FunctionDeclarations []funcDecl `json:"functionDeclarations,omitempty"`
}

type funcDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func Decode(r io.Reader, modelFromPath string) (*ir.Request, error) {
	var req request
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode gemini request: %w", err)
	}
	out := &ir.Request{
		Model:         modelFromPath,
		OriginalModel: modelFromPath,
		OriginalProto: "gemini",
	}
	if req.SystemInstruction != nil {
		var sb string
		for _, p := range req.SystemInstruction.Parts {
			sb += p.Text
		}
		out.System = sb
	}
	for _, c := range req.Contents {
		role := c.Role
		if role == "model" {
			role = "assistant"
		}
		if role == "" {
			role = "user"
		}
		msg := ir.Message{Role: ir.Role(role)}
		for _, p := range c.Parts {
			if p.Text != "" {
				msg.Parts = append(msg.Parts, ir.Part{Kind: ir.PartText, Text: p.Text})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	if req.GenerationConfig != nil {
		out.Temperature = req.GenerationConfig.Temperature
		out.TopP = req.GenerationConfig.TopP
		out.MaxTokens = req.GenerationConfig.MaxOutputTokens
	}
	for _, t := range req.Tools {
		for _, f := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, ir.ToolDef{
				Name: f.Name, Description: f.Description, Schema: []byte(f.Parameters),
			})
		}
	}
	return out, nil
}

type Encoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	stream  bool
}

func NewEncoder(w http.ResponseWriter, model string, stream bool) *Encoder {
	flusher, _ := w.(http.Flusher)
	return &Encoder{w: w, flusher: flusher, model: model, stream: stream}
}

func (e *Encoder) WriteHeaders() {
	if e.stream {
		e.w.Header().Set("Content-Type", "text/event-stream")
		e.w.Header().Set("Cache-Control", "no-cache, no-transform")
		e.w.Header().Set("X-Accel-Buffering", "no")
	} else {
		e.w.Header().Set("Content-Type", "application/json")
	}
	e.w.WriteHeader(http.StatusOK)
}

type respChunk struct {
	Candidates []candidate `json:"candidates"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
}

func (e *Encoder) Stream(events <-chan ir.Event) error {
	if !e.stream {
		return e.collect(events)
	}
	for ev := range events {
		switch ev.Kind {
		case ir.EvTextDelta:
			ch := respChunk{
				Candidates: []candidate{{
					Content: content{Role: "model", Parts: []part{{Text: ev.Text}}},
				}},
			}
			b, _ := json.Marshal(ch)
			fmt.Fprintf(e.w, "data: %s\n\n", b)
			if e.flusher != nil {
				e.flusher.Flush()
			}
		case ir.EvThinkingDelta:
			ch := respChunk{
				Candidates: []candidate{{
					Content: content{Role: "model", Parts: []part{{Text: ev.Text, Thought: true}}},
				}},
			}
			b, _ := json.Marshal(ch)
			fmt.Fprintf(e.w, "data: %s\n\n", b)
			if e.flusher != nil {
				e.flusher.Flush()
			}
		case ir.EvDone:
			ch := respChunk{
				Candidates: []candidate{{
					Content:      content{Role: "model", Parts: []part{}},
					FinishReason: ev.FinishReason,
				}},
			}
			b, _ := json.Marshal(ch)
			fmt.Fprintf(e.w, "data: %s\n\n", b)
			if e.flusher != nil {
				e.flusher.Flush()
			}
			return nil
		case ir.EvError:
			return ev.Err
		}
	}
	return nil
}

func (e *Encoder) collect(events <-chan ir.Event) error {
	var sb string
	var thinking string
	finish := "STOP"
	for ev := range events {
		switch ev.Kind {
		case ir.EvTextDelta:
			sb += ev.Text
		case ir.EvThinkingDelta:
			thinking += ev.Text
		case ir.EvDone:
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		case ir.EvError:
			return ev.Err
		}
	}
	parts := []part{}
	if thinking != "" {
		parts = append(parts, part{Text: thinking, Thought: true})
	}
	parts = append(parts, part{Text: sb})
	resp := respChunk{
		Candidates: []candidate{{
			Content:      content{Role: "model", Parts: parts},
			FinishReason: finish,
		}},
	}
	return json.NewEncoder(e.w).Encode(resp)
}
