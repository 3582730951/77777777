// cli_compat.go — compatibility endpoints for Codex CLI and Claude Code CLI.
//
// When users set OPENAI_BASE_URL (Codex) or anthropic base URL (Claude Code)
// to our gateway, the CLIs call auxiliary endpoints. This file implements those
// so the CLIs behave identically to the real upstream — including showing
// correct 5h / 7d quota windows in their status output.
package server

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/protocol/anthropic"
	"github.com/llm-pool/gateway/internal/tokenizer"
)

// ── Codex CLI ─────────────────────────────────────────────────────────────────

// handleCodexModels returns the Codex model list.
// Codex CLI: GET /backend-api/codex/models?client_version=0.45.0
func (g *Gateway) handleCodexModels(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)

	type codexModel struct {
		Slug  string   `json:"slug"`
		Title string   `json:"title"`
		Tags  []string `json:"tags,omitempty"`
	}

	models := []codexModel{}
	seen := map[string]bool{}
	add := func(slug string) {
		if !seen[slug] {
			models = append(models, codexModel{Slug: slug, Title: slug, Tags: []string{"chat", "code"}})
			seen[slug] = true
		}
	}

	if res.Group != nil {
		for _, m := range res.Group.Models {
			add(m)
		}
		for _, m := range res.Group.ModelWhitelist {
			add(m)
		}
		// Pull from discovered models in scheduler slots.
		if len(models) == 0 {
			for _, sl := range g.sched.Snapshot() {
				if sl.Provider != res.Group.Provider {
					continue
				}
				for _, m := range sl.DiscoveredModels {
					add(m)
				}
			}
		}
	}
	if len(models) == 0 {
		for _, m := range []string{"gpt-5.2", "gpt-5.3", "gpt-5.4"} {
			add(m)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleCodexConversationLimit returns the 5h / 7d quota window status.
// Codex CLI: GET /backend-api/conversation_limit
//
// Real ChatGPT response shape (captured from Plus account):
//
//	{
//	  "message_cap":       { "limit": 0,   "remaining": 0,   "reset_time_utc": null },
//	  "message_cap_ffp":   { "limit": 150, "remaining": 112, "reset_time_utc": "2026-05-01T18:00:00Z" },
//	  "message_cap_ffp_7d":{ "limit": 500, "remaining": 437, "reset_time_utc": "2026-05-07T00:00:00Z" },
//	  "value": "not_limited"
//	}
func (g *Gateway) handleCodexConversationLimit(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)

	var shortUsed, shortLimit, longUsed, longLimit float64
	var shortReset, longReset time.Time

	if res.Group != nil {
		// Find the account with the highest usage percentage (worst case).
		// This ensures CLI shows "limited" when ANY account is near exhaustion.
		var worstShortPct, worstLongPct float64 = -1, -1
		for _, sl := range g.sched.Snapshot() {
			if sl.Provider != res.Group.Provider || sl.QuotaShortLimit <= 0 {
				continue
			}
			sPct := sl.QuotaShortUsed / sl.QuotaShortLimit
			if sPct > worstShortPct {
				worstShortPct = sPct
				shortUsed = sl.QuotaShortUsed
				shortLimit = sl.QuotaShortLimit
				if !sl.QuotaShortReset.IsZero() {
					shortReset = sl.QuotaShortReset
				}
			}
			if sl.QuotaLongLimit > 0 {
				lPct := sl.QuotaLongUsed / sl.QuotaLongLimit
				if lPct > worstLongPct {
					worstLongPct = lPct
					longUsed = sl.QuotaLongUsed
					longLimit = sl.QuotaLongLimit
					if !sl.QuotaLongReset.IsZero() {
						longReset = sl.QuotaLongReset
					}
				}
			}
		}
	}

	if shortLimit == 0 {
		shortLimit = 150
	}
	if longLimit == 0 {
		longLimit = 500
	}
	if shortReset.IsZero() {
		shortReset = time.Now().Add(5 * time.Hour)
	}
	if longReset.IsZero() {
		longReset = time.Now().Add(7 * 24 * time.Hour)
	}

	shortRem := shortLimit - shortUsed
	if shortRem < 0 {
		shortRem = 0
	}
	longRem := longLimit - longUsed
	if longRem < 0 {
		longRem = 0
	}

	value := "not_limited"
	if shortRem == 0 {
		value = "limited"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message_cap": map[string]any{
			"limit": 0, "remaining": 0, "reset_time_utc": nil,
		},
		"message_cap_ffp": map[string]any{
			"limit":          shortLimit,
			"remaining":      shortRem,
			"reset_time_utc": shortReset.UTC().Format(time.RFC3339),
		},
		"message_cap_ffp_7d": map[string]any{
			"limit":          longLimit,
			"remaining":      longRem,
			"reset_time_utc": longReset.UTC().Format(time.RFC3339),
		},
		"value": value,
	})
}

// ── Claude Code CLI ───────────────────────────────────────────────────────────

// handleClaudeOrganizations satisfies GET /v1/organizations which Claude Code
// CLI calls to discover org_uuid.
func (g *Gateway) handleClaudeOrganizations(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	tenantID := "default"
	if res.Group != nil {
		tenantID = res.Group.TenantID
	}
	writeJSON(w, http.StatusOK, []map[string]any{
		{
			"uuid":         "pool-org-" + tenantID,
			"name":         "LLM Pool",
			"raven_type":   nil,
			"capabilities": []string{"claude_code"},
		},
	})
}

// handleClaudeUsage satisfies GET /v1/usage.
func (g *Gateway) handleClaudeUsage(w http.ResponseWriter, r *http.Request) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var input, output int64
	if g.store != nil {
		input, output, _ = g.store.QueryDailyTokenUsage(r.Context(), today)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"period":        "daily",
		"period_start":  today.Format(time.RFC3339),
	})
}

// ── Unified /v1/models with UA-based routing ──────────────────────────────────

// handleListModels returns the live model list for the calling client.
//
// Priority: group whitelist → account-discovered models (real-time from Discover)
// → provider-specific defaults.
//
// For Claude Code using /model to switch providers: when the group.Provider is
// "chatgpt" or "gemini" but the request comes from a claude-cli UA, we return
// OpenAI/Gemini models in the list and Claude Code can pick them with /model.
// The Anthropic-protocol request then routes through IR to the correct provider.
func (g *Gateway) handleListModels(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	ua := r.Header.Get("User-Agent")
	_ = strings.HasPrefix(ua, "claude-cli/") // UA available for future routing

	var data []map[string]any
	seen := map[string]bool{}
	addModel := func(id, ownedBy string, caps ...map[string]any) {
		if !seen[id] {
			m := map[string]any{
				"id": id, "object": "model", "owned_by": ownedBy, "created": 1700000000,
			}
			if len(caps) > 0 {
				for k, v := range caps[0] {
					m[k] = v
				}
			}
			data = append(data, m)
			seen[id] = true
		}
	}

	if res.Group != nil {
		provider := res.Group.Provider

		// Step 1: Group explicit whitelist (highest priority — admin curated).
		for _, m := range res.Group.Models {
			addModel(m, provider)
		}
		for _, m := range res.Group.ModelWhitelist {
			addModel(m, provider)
		}

		// Step 2: Live discovered models from account pool (real-time).
		// AvailableModels returns the union of all accounts that have completed
		// at least one Discover() call — always up to date.
		for _, mc := range g.sched.AvailableModels(provider) {
			caps := map[string]any{
				"context_window":  mc.ContextWindow,
				"supports_tools":  mc.SupportsTools,
				"supports_vision": mc.SupportsVision,
			}
			addModel(mc.ID, provider, caps)
		}
	}

	// Step 3: Fallback defaults when pool is empty or discovery pending.
	if len(data) == 0 {
		provider := "openai"
		if res.Group != nil {
			provider = res.Group.Provider
		}
		switch provider {
		case "claude":
			for _, m := range []string{
				"claude-opus-4-7", "claude-sonnet-4-6", "claude-sonnet-4-5-20250929",
				"claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022",
			} {
				addModel(m, "anthropic")
			}
		case "gemini":
			for _, m := range []string{
				"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite",
				"gemini-3-pro-preview", "gemini-3-flash-preview",
			} {
				addModel(m, "google")
			}
		default:
			for _, m := range []string{
				"gpt-5.2", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5",
			} {
				addModel(m, "openai")
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleCountTokens estimates input token count for an Anthropic-format request.
// POST /v1/messages/count_tokens — used by Claude Code for context window management.
func (g *Gateway) handleCountTokens(w http.ResponseWriter, r *http.Request) {
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
	releaseBody()
	body = nil
	count := tokenizer.EstimateRequest(req)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": count})
}
