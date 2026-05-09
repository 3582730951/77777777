package server

import (
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func applyGroupSystemPrompt(req *ir.Request, group *domain.Group) {
	if req == nil || group == nil || group.SystemPrompt == "" {
		return
	}
	req.System = combineSystemPrompt(req.System, group.SystemPrompt, group.SystemPromptMode)
}
