package normalize

import (
	"encoding/json"
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func TestNormalizeSystemPrompt_Empty(t *testing.T) {
	if got := NormalizeSystemPrompt(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestNormalizeSystemPrompt_TrailingSpaces(t *testing.T) {
	got := NormalizeSystemPrompt("hello   \nworld  \t")
	if got != "hello\nworld" {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeSystemPrompt_CollapseBlankLines(t *testing.T) {
	got := NormalizeSystemPrompt("a\n\n\n\nb")
	if got != "a\n\nb" {
		t.Errorf("expected single blank line, got %q", got)
	}
}

func TestNormalizeSystemPrompt_TrimLeadingTrailingBlanks(t *testing.T) {
	got := NormalizeSystemPrompt("\n\nhello\n\n")
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeSystemPrompt_Idempotent(t *testing.T) {
	input := "hello  \n\n\n\nworld  "
	first := NormalizeSystemPrompt(input)
	second := NormalizeSystemPrompt(first)
	if first != second {
		t.Errorf("not idempotent: %q vs %q", first, second)
	}
}

func TestNormalizeJSONSchema_SortsKeys(t *testing.T) {
	input := []byte(`{"z":1,"a":2}`)
	got := NormalizeJSONSchema(input)
	want := `{"a":2,"z":1}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestNormalizeJSONSchema_NestedObjects(t *testing.T) {
	input := []byte(`{"b":{"z":1,"a":2},"a":3}`)
	got := NormalizeJSONSchema(input)
	want := `{"a":3,"b":{"a":2,"z":1}}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestNormalizeJSONSchema_Empty(t *testing.T) {
	got := NormalizeJSONSchema(nil)
	if got != nil {
		t.Error("nil should return nil")
	}
	got = NormalizeJSONSchema([]byte{})
	if len(got) != 0 {
		t.Error("empty should return empty")
	}
}

func TestNormalizeJSONSchema_InvalidJSON(t *testing.T) {
	input := []byte(`not json`)
	got := NormalizeJSONSchema(input)
	if string(got) != "not json" {
		t.Error("invalid JSON should be returned as-is")
	}
}

func TestSortTools(t *testing.T) {
	tools := []ir.ToolDef{
		{Name: "zebra"},
		{Name: "alpha"},
		{Name: "mid"},
	}
	SortTools(tools)
	if tools[0].Name != "alpha" || tools[1].Name != "mid" || tools[2].Name != "zebra" {
		t.Errorf("tools not sorted: %v", []string{tools[0].Name, tools[1].Name, tools[2].Name})
	}
}

func TestSortTools_Stable(t *testing.T) {
	tools := []ir.ToolDef{
		{Name: "a", Description: "first"},
		{Name: "a", Description: "second"},
	}
	SortTools(tools)
	if tools[0].Description != "first" {
		t.Error("sort should be stable")
	}
}

func TestRequest_Integration(t *testing.T) {
	req := &ir.Request{
		System: "hello  \n\n\n",
		Tools: []ir.ToolDef{
			{Name: "b", Schema: []byte(`{"z":1,"a":2}`)},
			{Name: "a", Schema: []byte(`{"x":1}`)},
		},
	}
	Request(req)
	if req.System != "hello" {
		t.Errorf("system not normalized: %q", req.System)
	}
	if req.Tools[0].Name != "a" {
		t.Error("tools not sorted")
	}
}

func TestHashConversation_Deterministic(t *testing.T) {
	mk := func() *ir.Request {
		return &ir.Request{
			System: "sys",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "hi"}}},
				{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}}},
				{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "bye"}}},
			},
		}
	}
	h1 := HashConversation(mk())
	h2 := HashConversation(mk())
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("expected 16-char hex, got %d chars", len(h1))
	}
}

func TestHashConversation_SingleMessage(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{{Role: ir.RoleUser}},
	}
	if h := HashConversation(req); h != "" {
		t.Errorf("single message should return empty hash, got %q", h)
	}
}

func TestHashConversation_DifferentLastMessage(t *testing.T) {
	r1 := &ir.Request{
		System: "sys",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}}},
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "msg A"}}},
		},
	}
	r2 := &ir.Request{
		System: "sys",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartText, Text: "hello"}}},
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "msg B"}}},
		},
	}
	h1 := HashConversation(r1)
	h2 := HashConversation(r2)
	if h1 != h2 {
		t.Error("same prefix with different last message should produce same hash")
	}
}

func TestHashConversation_ToolParts(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Parts: []ir.Part{
				{Kind: ir.PartToolUse, ToolUseID: "t1", ToolUseName: "read", ToolUseInput: []byte(`{"path":"a"}`)},
			}},
			{Role: ir.RoleTool, Parts: []ir.Part{
				{Kind: ir.PartToolResult, ToolResultID: "t1", ToolResultBytes: []byte("content")},
			}},
			{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "next"}}},
		},
	}
	h := HashConversation(req)
	if h == "" {
		t.Error("should produce non-empty hash")
	}
}

func TestInjectAnthropicCacheBreakpoints_StringSystem(t *testing.T) {
	body := []byte(`{"system":"hello","messages":[]}`)
	got := InjectAnthropicCacheBreakpoints(body)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(parsed["system"], &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	cc, ok := blocks[0]["cache_control"]
	if !ok {
		t.Fatal("missing cache_control")
	}
	ccMap := cc.(map[string]any)
	if ccMap["type"] != "ephemeral" {
		t.Errorf("expected ephemeral, got %v", ccMap["type"])
	}
}

func TestInjectAnthropicCacheBreakpoints_AlreadyMarked(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`)
	got := InjectAnthropicCacheBreakpoints(body)
	if string(got) != string(body) {
		t.Error("should not modify already-marked system")
	}
}

func TestInjectAnthropicCacheBreakpoints_ToolsBreakpoint(t *testing.T) {
	body := []byte(`{"tools":[{"name":"a"},{"name":"b"}]}`)
	got := InjectAnthropicCacheBreakpoints(body)
	var parsed map[string]json.RawMessage
	json.Unmarshal(got, &parsed)
	var tools []map[string]json.RawMessage
	json.Unmarshal(parsed["tools"], &tools)
	if _, has := tools[0]["cache_control"]; has {
		t.Error("first tool should not have cache_control")
	}
	if _, has := tools[1]["cache_control"]; !has {
		t.Error("last tool should have cache_control")
	}
}

func TestInjectAnthropicCacheBreakpoints_Empty(t *testing.T) {
	got := InjectAnthropicCacheBreakpoints(nil)
	if got != nil {
		t.Error("nil input should return nil")
	}
	got = InjectAnthropicCacheBreakpoints([]byte{})
	if len(got) != 0 {
		t.Error("empty input should return empty")
	}
}
