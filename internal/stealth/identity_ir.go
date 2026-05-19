package stealth

import (
	"encoding/json"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

// RewriteIRRequest rewrites every user-visible string carried in the IR request.
// It leaves model names, roles, and tool names unchanged.
func (r *IdentityRewriter) RewriteIRRequest(req *ir.Request) {
	if r == nil || req == nil {
		return
	}
	req.System = r.ReplaceString(req.System)
	req.AnthropicSystemText = r.ReplaceString(req.AnthropicSystemText)
	req.AnthropicSystem = r.rewriteJSONishBytes(req.AnthropicSystem)
	req.AnthropicMetadata = r.rewriteJSONishBytes(req.AnthropicMetadata)
	req.AnthropicContextManagement = r.rewriteJSONishBytes(req.AnthropicContextManagement)
	req.AnthropicToolChoice = r.rewriteJSONishBytes(req.AnthropicToolChoice)
	for i := range req.Messages {
		for j := range req.Messages[i].Parts {
			p := &req.Messages[i].Parts[j]
			switch p.Kind {
			case ir.PartText:
				p.Text = r.ReplaceString(p.Text)
			case ir.PartImage:
				p.ImageURL = r.ReplaceString(p.ImageURL)
			case ir.PartToolUse:
				p.ToolUseInput = r.rewriteJSONishBytes(p.ToolUseInput)
			case ir.PartToolResult:
				p.ToolResultBytes = r.rewriteJSONishBytes(p.ToolResultBytes)
			}
		}
	}
	for i := range req.Tools {
		req.Tools[i].Description = r.ReplaceString(req.Tools[i].Description)
		req.Tools[i].Schema = r.rewriteJSONishBytes(req.Tools[i].Schema)
	}
}

func (r *IdentityRewriter) rewriteJSONishBytes(b []byte) []byte {
	if r == nil || len(b) == 0 {
		return b
	}
	if json.Valid(b) {
		return r.RewriteJSONBody(b)
	}
	next := r.ReplaceString(string(b))
	if next == string(b) {
		return b
	}
	return []byte(next)
}
