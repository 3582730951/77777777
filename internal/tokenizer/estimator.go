// Package tokenizer provides a fast model-agnostic estimator for input token
// counts. It runs entirely locally with no model files; we use this estimate
// purely for routing decisions ("avoid an account that's about to overflow")
// and never display it to the user as authoritative.
//
// Heuristics:
//   - English / Latin text: ~4 chars per token (matches GPT-style BPE in practice)
//   - CJK / Japanese / Korean: ~1.5 chars per token (each char tends to be its own token)
//   - JSON / structured tool args: discount whitespace, count keys + values
//   - Images / file blobs: provider-billed but we cap at known constants
package tokenizer

import (
	"unicode"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

// EstimateRequest gives a cheap upper-bound estimate of the input tokens for a
// request. Used by the scheduler to deprioritise accounts whose remaining
// quota wouldn't cover this call.
func EstimateRequest(req *ir.Request) int {
	total := 0
	total += estimateText(req.System)
	for _, m := range req.Messages {
		// role tag overhead
		total += 4
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.PartText:
				total += estimateText(p.Text)
			case ir.PartImage:
				if len(p.ImageBytes) > 0 {
					// Images: GPT-4o ~85 tokens base + 170 per 512x512 tile.
					// We don't know dims here without decode; use a flat estimate.
					total += 765
				} else if p.ImageURL != "" {
					total += 85
				}
			case ir.PartToolUse:
				total += estimateText(p.ToolUseName) + estimateText(string(p.ToolUseInput)) + 8
			case ir.PartToolResult:
				total += estimateText(string(p.ToolResultBytes)) + 6
			}
		}
	}
	for _, t := range req.Tools {
		total += estimateText(t.Name) + estimateText(t.Description) + estimateText(string(t.Schema)) + 12
	}
	if req.MaxTokens > 0 {
		total += req.MaxTokens
	}
	return total
}

func estimateText(s string) int {
	if s == "" {
		return 0
	}
	cjk := 0
	other := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else if !unicode.IsSpace(r) {
			other++
		}
	}
	// CJK ~ 1 token per char (slight overestimate, safer for routing).
	// Latin ~ 4 chars per token.
	return cjk + (other+3)/4
}
