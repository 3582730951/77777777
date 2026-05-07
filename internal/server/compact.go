// compact.go — auto-compaction endpoints for Codex (OpenAI CLI) and Claude Code.
//
// # How Codex CLI triggers compact
//
// When OPENAI_BASE_URL is set to our gateway, Codex CLI calls:
//
//	POST {base_url}/backend-api/codex/responses/compact
//
// with an OpenAI Responses API body (input[] array, not messages[]).
// It expects a non-streaming JSON back. We detect both Responses API shape
// (has "input" key) and Chat Completions shape (has "messages" key), decode
// the appropriate format, inject a compact system prompt, and return the
// summary in whichever format the client sent.
//
// # How Claude Code triggers compact
//
// Claude Code sends:
//
//	POST {base_url}/v1/messages
//
// with a special system prompt starting with "You are a helpful AI assistant
// tasked with summarizing conversations." — the same /v1/messages endpoint
// we already serve. For the dedicated compact path (v1/claude-code/compact),
// we additionally inject our own higher-quality system prompt.
//
// # Cross-provider routing (Claude Code + /model → OpenAI/Gemini)
//
// Because all protocols decode to the same IR, Claude Code can use any provider:
//
//	Claude Code ──Anthropic wire──▶ /v1/messages ──IR──▶ chatgpt/gemini provider
//	                                 ↑ inboundProto="anthropic"
//	                                 ↑ res.Group.Provider="chatgpt" ← from API Key group
//
// The user sets ANTHROPIC_BASE_URL to our gateway and uses an API Key that
// belongs to an "openai" or "gemini" group. Then:
//   - /model gpt-5.2  → model rewrites to gpt-5.2, routes to chatgpt provider
//   - /model gemini-2.5-pro → routes to gemini provider
//   - Response is always Anthropic SSE (inboundProto is "anthropic")
//
// This is the native cross-provider feature — no code changes needed.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/protocol/anthropic"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/protocol/openai"
)

const (
	codexCompactDefaultModel  = "gpt-5.2" // cheapest Codex model for summarization
	claudeCompactDefaultModel = "claude-haiku-4-5-20251001"
	purposeCompactHeader      = "x-pool-purpose"

	// compactSystemPromptCodex is the system prompt injected for Codex compact.
	// Goals: tight token budget, preserve code/paths verbatim, keep last 2 turns.
	compactSystemPromptCodex = `You are a conversation compressor for a coding session.
Given the conversation history below, produce a compact context block (target 300-600 tokens):
- Preserve ALL: file paths, code snippets, commands, error messages, model names — verbatim
- Preserve ALL: user instructions, constraints, preferences, decisions made
- Write in third-person past tense
- End with "==CONVERSATION SO FAR==" followed by the last 2 turns verbatim (do NOT omit)
Do NOT answer questions. Output only the compact summary block.`

	// compactSystemPromptClaude is the system prompt injected for Claude Code compact.
	compactSystemPromptClaude = `You are an expert conversation summarizer for Claude Code's auto-compact feature.
Compress the conversation below into a tight context block (~400 tokens):
- Preserve ALL: file paths, code, commands, error messages verbatim
- Preserve ALL: user instructions, constraints, preferences
- Preserve: unresolved questions, pending TODOs
- End with "<<RECENT_TURNS>>" followed by the last 2 user/assistant turns verbatim
Do NOT answer. Output only the summary block.`
)

// handleCodexCompact serves POST /backend-api/codex/responses/compact and
// POST /v1/codex/compact.
//
// Codex CLI sends either Responses API format (with "input" key) or Chat
// Completions format (with "messages" key). We detect and handle both.
// The response is always non-streaming OpenAI Chat Completions JSON so the
// CLI can extract the summary text.
func (g *Gateway) handleCodexCompact(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()

	// Detect format by presence of "input" vs "messages" key.
	var req *ir.Request
	if isResponsesAPIBody(body) {
		// Codex CLI sends Responses API shape: {model, input[], instructions, ...}
		req, err = decodeResponsesAPICompact(body)
	} else {
		req, err = openai.DecodeBytes(body)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil

	if req.Model == "" || req.Model == "auto" {
		req.Model = codexCompactDefaultModel
		req.OriginalModel = codexCompactDefaultModel
	}
	// Prepend our compact system prompt; preserve any existing system context.
	if req.System == "" {
		req.System = compactSystemPromptCodex
	} else {
		req.System = compactSystemPromptCodex + "\n\n---\nAdditional context:\n" + req.System
	}
	// Compact is always non-streaming — CLI collects the full JSON.
	req.Stream = false

	w.Header().Set(purposeCompactHeader, "codex")
	g.serveRequest(w, r, res, req, "openai")
}

// handleClaudeCodeCompact serves POST /v1/claude-code/compact.
//
// Claude Code uses Anthropic Messages API format and expects Anthropic SSE back.
func (g *Gateway) handleClaudeCodeCompact(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	req, err := anthropic.Decode(bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil
	if req.Model == "" || req.Model == "auto" {
		req.Model = claudeCompactDefaultModel
		req.OriginalModel = claudeCompactDefaultModel
	}
	if req.System == "" {
		req.System = compactSystemPromptClaude
	} else {
		req.System = compactSystemPromptClaude + "\n\n---\n" + req.System
	}
	w.Header().Set(purposeCompactHeader, "claude-code")
	// inboundProto="anthropic" → response is Anthropic SSE
	g.serveRequest(w, r, res, req, "anthropic")
}

// handleCompactGeneric auto-detects by anthropic-version header or ?style= query.
func (g *Gateway) handleCompactGeneric(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("anthropic-version"), "2023") ||
		r.URL.Query().Get("style") == "anthropic" ||
		strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") {
		g.handleClaudeCodeCompact(w, r)
		return
	}
	g.handleCodexCompact(w, r)
}

// handleCompactSummary: one-shot non-streaming summary for orchestrators.
// Request: {messages, model?, max_tokens?, preserve_recent?}
// Response: {summary, tokens_input, tokens_output, upstream_model}
func (g *Gateway) handleCompactSummary(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	req, err := openai.DecodeBytes(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	body = nil
	if req.Model == "" {
		req.Model = codexCompactDefaultModel
		req.OriginalModel = codexCompactDefaultModel
	}
	if req.System == "" {
		req.System = compactSystemPromptCodex
	} else {
		req.System = compactSystemPromptCodex + "\n\n---\n" + req.System
	}
	req.Stream = false
	if req.MaxTokens == 0 {
		req.MaxTokens = 2048
	}
	w.Header().Set(purposeCompactHeader, "summary")
	g.serveRequest(w, r, res, req, "openai")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// isResponsesAPIBody returns true if the body uses OpenAI Responses API format
// (has top-level "input" key) rather than Chat Completions ("messages" key).
func isResponsesAPIBody(body []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	_, hasInput := probe["input"]
	return hasInput
}

// decodeResponsesAPICompact converts a Codex Responses API compact body to IR.
// The body shape is:
//
//	{ "model": "gpt-5.2",
//	  "input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]},...],
//	  "instructions": "..." }
func decodeResponsesAPICompact(body []byte) (*ir.Request, error) {
	var raw struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&raw); err != nil {
		return nil, err
	}

	req := &ir.Request{
		Model:         raw.Model,
		OriginalModel: raw.Model,
		System:        raw.Instructions,
		Stream:        false,
	}

	for _, item := range raw.Input {
		if item.Type != "message" {
			continue
		}
		var role ir.Role
		switch item.Role {
		case "user":
			role = ir.RoleUser
		case "assistant":
			role = ir.RoleAssistant
		case "system":
			role = ir.RoleSystem
		default:
			continue
		}
		var parts []ir.Part
		for _, c := range item.Content {
			if c.Text != "" {
				parts = append(parts, ir.Part{Kind: ir.PartText, Text: c.Text})
			}
		}
		if len(parts) > 0 {
			req.Messages = append(req.Messages, ir.Message{Role: role, Parts: parts})
		}
	}
	return req, nil
}

// Keep unused import from being tree-shaken.
var _ = ir.RoleAssistant
