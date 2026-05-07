// Package router applies cross-cutting request transforms before scheduling:
// model alias rewriting (per-apikey > per-group), model whitelist enforcement,
// and conversation hashing for sticky routing.
package router

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/protocol/ir"
)

var ErrModelNotAllowed = errors.New("model_not_allowed")

// RewriteModel applies the alias precedence chain: per-apikey > per-group.
// Returns the (possibly rewritten) effective model and an error if the
// resulting model is not on the group whitelist.
// Federation mode skips aliases and whitelist — the original model name is
// passed through so the scheduler can match it against DiscoveredModels
// across all member groups.
func RewriteModel(req *ir.Request, res auth.Resolved) (effective string, err error) {
	effective = req.Model

	if res.Federation != nil {
		return effective, nil
	}

	if res.APIKeyOverride.ModelAliases != nil {
		if v, ok := res.APIKeyOverride.ModelAliases[effective]; ok && v != "" {
			effective = v
		}
	}
	if res.Group != nil && res.Group.ModelAliases != nil {
		if v, ok := res.Group.ModelAliases[effective]; ok && v != "" {
			effective = v
		}
	}

	if res.Group != nil && len(res.Group.ModelWhitelist) > 0 {
		ok := false
		for _, m := range res.Group.ModelWhitelist {
			if m == effective {
				ok = true
				break
			}
		}
		if !ok {
			return effective, ErrModelNotAllowed
		}
	}
	return effective, nil
}

// HashConversation produces a stable identifier for the conversation prefix
// so the scheduler can apply sticky routing. The hash covers the system
// prompt + every message except the last (which is the new turn).
func HashConversation(req *ir.Request) string {
	if len(req.Messages) <= 1 {
		return ""
	}
	h := xxhash.New()
	h.Write([]byte(req.System))
	h.Write([]byte{'\x00'})
	for i := 0; i < len(req.Messages)-1; i++ {
		m := req.Messages[i]
		h.Write([]byte(m.Role))
		h.Write([]byte{'\x01'})
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.PartText:
				h.Write([]byte(p.Text))
			case ir.PartToolUse:
				h.Write([]byte(p.ToolUseID))
				h.Write([]byte(p.ToolUseName))
				h.Write(p.ToolUseInput)
			case ir.PartToolResult:
				h.Write([]byte(p.ToolResultID))
				h.Write(p.ToolResultBytes)
			}
			h.Write([]byte{'\x02'})
		}
		h.Write([]byte{'\x03'})
	}
	_ = binary.LittleEndian // keep import
	return fmt.Sprintf("%016x%016x", h.Sum64(), h.Sum64())[:32]
}

// DetectInboundProtocol guesses the wire protocol from the request path.
func DetectInboundProtocol(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic"
	case strings.HasPrefix(path, "/v1beta/"):
		return "gemini"
	case strings.HasPrefix(path, "/v1/chat/completions"), strings.HasPrefix(path, "/v1/completions"):
		return "openai"
	}
	return ""
}
