package router_test

import (
	"testing"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/router"
)

func TestModelRewriteGroupAlias(t *testing.T) {
	res := auth.Resolved{
		Group: &domain.Group{
			ID:           "g1",
			ModelAliases: map[string]string{"gpt-5.3": "gpt-5.4"},
			ModelWhitelist: []string{"gpt-5.4"},
		},
	}
	req := &ir.Request{Model: "gpt-5.3"}
	got, err := router.RewriteModel(req, res)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got != "gpt-5.4" {
		t.Errorf("want gpt-5.4 got %s", got)
	}
}

func TestModelRewriteAPIKeyOverridesGroup(t *testing.T) {
	res := auth.Resolved{
		APIKey: "vip",
		APIKeyOverride: domain.APIKeyOverride{
			ModelAliases: map[string]string{"gpt-5.3": "o1"},
		},
		Group: &domain.Group{
			ID:           "g1",
			ModelAliases: map[string]string{"gpt-5.3": "gpt-5.4"},
			ModelWhitelist: []string{"o1", "gpt-5.4"},
		},
	}
	req := &ir.Request{Model: "gpt-5.3"}
	got, _ := router.RewriteModel(req, res)
	if got != "o1" {
		t.Errorf("apikey should win; want o1 got %s", got)
	}
}

func TestModelWhitelistRejects(t *testing.T) {
	res := auth.Resolved{
		Group: &domain.Group{
			ID:             "g1",
			ModelWhitelist: []string{"gpt-4o"},
		},
	}
	req := &ir.Request{Model: "claude-opus"}
	if _, err := router.RewriteModel(req, res); err == nil {
		t.Errorf("expected whitelist rejection")
	}
}

func TestHashConversationStable(t *testing.T) {
	req := &ir.Request{
		System: "you are bot",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}}},
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "new turn"}}},
		},
	}
	h1 := router.HashConversation(req)
	if h1 == "" {
		t.Errorf("expected non-empty hash")
	}
	// Hash of same prefix (different last turn) should be the same.
	req2 := &ir.Request{System: req.System, Messages: append([]ir.Message{}, req.Messages...)}
	req2.Messages[2] = ir.Message{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "different last turn"}}}
	h2 := router.HashConversation(req2)
	if h1 != h2 {
		t.Errorf("hash should be invariant over last message; %s vs %s", h1, h2)
	}
}
