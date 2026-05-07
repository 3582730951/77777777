package stealth

import (
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func TestToolRewriterSkipsSmallToolSets(t *testing.T) {
	tw := NewToolRewriter([]ir.ToolDef{
		{Name: "Bash"},
		{Name: "Read"},
		{Name: "Write"},
	})
	if tw != nil {
		t.Fatal("small tool set should not be rewritten")
	}
}

func TestToolRewriterPreservesCanonicalCLITools(t *testing.T) {
	tw := NewToolRewriter([]ir.ToolDef{
		{Name: "Bash"},
		{Name: "Read"},
		{Name: "Write"},
		{Name: "Edit"},
		{Name: "TodoWrite"},
		{Name: "WebFetch"},
	})
	if tw != nil {
		t.Fatal("canonical CLI tools should not be rewritten")
	}
}

func TestToolRewriterOnlyRewritesNonCanonicalTools(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.ToolDef{
			{Name: "Bash"},
			{Name: "Read"},
			{Name: "Write"},
			{Name: "Edit"},
			{Name: "TodoWrite"},
			{Name: "internal_proxy_tool"},
		},
		ToolChoice: ir.ToolChoice{Name: "internal_proxy_tool"},
		Messages: []ir.Message{{
			Role: ir.RoleAssistant,
			Parts: []ir.Part{{
				Kind:        ir.PartToolUse,
				ToolUseName: "internal_proxy_tool",
			}},
		}},
	}
	tw := NewToolRewriter(req.Tools)
	if tw == nil {
		t.Fatal("expected non-canonical tool rewriter")
	}

	tw.Apply(req)

	if req.Tools[0].Name != "Bash" {
		t.Fatalf("canonical tool changed: %q", req.Tools[0].Name)
	}
	fake := req.Tools[5].Name
	if fake == "internal_proxy_tool" || fake == "" {
		t.Fatalf("non-canonical tool was not rewritten: %q", fake)
	}
	if req.ToolChoice.Name != fake {
		t.Fatalf("tool choice not rewritten: %q want %q", req.ToolChoice.Name, fake)
	}
	if req.Messages[0].Parts[0].ToolUseName != fake {
		t.Fatalf("message tool use not rewritten: %q want %q", req.Messages[0].Parts[0].ToolUseName, fake)
	}
	if got := string(tw.Restore([]byte(fake))); got != "internal_proxy_tool" {
		t.Fatalf("restore = %q", got)
	}
}
