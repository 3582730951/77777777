package cacheopt

import (
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func TestApply_NilRequest(t *testing.T) {
	Apply(nil, "claude") // should not panic
}

func TestNormaliseTools_Empty(t *testing.T) {
	NormaliseTools(nil)
	NormaliseTools([]ir.ToolDef{})
}

func TestNormaliseTools_SortsKeys(t *testing.T) {
	tools := []ir.ToolDef{
		{Name: "t1", Schema: []byte(`{"b":1,"a":2}`)},
	}
	NormaliseTools(tools)
	got := string(tools[0].Schema)
	want := `{"a":2,"b":1}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestNormaliseTools_AlreadyCanonical(t *testing.T) {
	orig := []byte(`{"a":1,"b":2}`)
	tools := []ir.ToolDef{{Name: "t", Schema: orig}}
	NormaliseTools(tools)
	if &tools[0].Schema[0] != &orig[0] {
		t.Error("expected no reallocation for already-canonical JSON")
	}
}

func TestNormaliseTools_InvalidJSON(t *testing.T) {
	orig := []byte(`not json`)
	tools := []ir.ToolDef{{Name: "t", Schema: orig}}
	NormaliseTools(tools)
	if string(tools[0].Schema) != "not json" {
		t.Error("invalid JSON should be left unchanged")
	}
}

func TestNormaliseTools_NestedSort(t *testing.T) {
	tools := []ir.ToolDef{
		{Name: "t", Schema: []byte(`{"z":{"b":1,"a":2},"a":3}`)},
	}
	NormaliseTools(tools)
	want := `{"a":3,"z":{"a":2,"b":1}}`
	if got := string(tools[0].Schema); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestNormaliseTools_EmptySchema(t *testing.T) {
	tools := []ir.ToolDef{{Name: "t", Schema: nil}}
	NormaliseTools(tools)
	if tools[0].Schema != nil {
		t.Error("nil schema should stay nil")
	}
}

func TestInjectCacheBreakpoints_ShortSystem(t *testing.T) {
	req := &ir.Request{System: "hi"}
	InjectCacheBreakpoints(req)
	if req.SystemCached {
		t.Error("short system should not be cached")
	}
}

func TestInjectCacheBreakpoints_LongSystem(t *testing.T) {
	req := &ir.Request{System: string(make([]byte, 801))}
	InjectCacheBreakpoints(req)
	if !req.SystemCached {
		t.Error("long system should be cached")
	}
}

func TestInjectCacheBreakpoints_LastTool(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.ToolDef{{Name: "a"}, {Name: "b"}},
	}
	InjectCacheBreakpoints(req)
	if req.Tools[0].CacheBreakpoint {
		t.Error("first tool should not have breakpoint")
	}
	if !req.Tools[1].CacheBreakpoint {
		t.Error("last tool should have breakpoint")
	}
}

func TestInjectCacheBreakpoints_NoTools(t *testing.T) {
	req := &ir.Request{}
	InjectCacheBreakpoints(req) // should not panic
}

func TestInjectCacheBreakpoints_MessageCacheIdx(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleUser},
			{Role: ir.RoleAssistant},
			{Role: ir.RoleUser},
			{Role: ir.RoleAssistant},
			{Role: ir.RoleUser},
		},
	}
	InjectCacheBreakpoints(req)
	if req.MessageCacheIdx != 2 {
		t.Errorf("expected MessageCacheIdx=2, got %d", req.MessageCacheIdx)
	}
}

func TestInjectCacheBreakpoints_TooFewMessages(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{{Role: ir.RoleUser}},
	}
	InjectCacheBreakpoints(req)
	if req.MessageCacheIdx != 0 {
		t.Errorf("expected MessageCacheIdx=0 with single message, got %d", req.MessageCacheIdx)
	}
}

func TestInjectCacheBreakpoints_NoUserMessages(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleAssistant},
			{Role: ir.RoleAssistant},
			{Role: ir.RoleAssistant},
		},
	}
	InjectCacheBreakpoints(req)
	if req.MessageCacheIdx != 0 {
		t.Errorf("expected MessageCacheIdx=0 with no user messages, got %d", req.MessageCacheIdx)
	}
}

func TestApply_ClaudeProvider(t *testing.T) {
	req := &ir.Request{
		System: string(make([]byte, 900)),
		Tools:  []ir.ToolDef{{Name: "t", Schema: []byte(`{"b":1,"a":2}`)}},
	}
	Apply(req, "claude")
	if !req.SystemCached {
		t.Error("expected SystemCached for claude")
	}
	if !req.Tools[0].CacheBreakpoint {
		t.Error("expected CacheBreakpoint for claude")
	}
}

func TestApply_NonClaudeProvider(t *testing.T) {
	req := &ir.Request{
		System: string(make([]byte, 900)),
		Tools:  []ir.ToolDef{{Name: "t", Schema: []byte(`{"b":1,"a":2}`)}},
	}
	Apply(req, "gemini")
	if req.SystemCached {
		t.Error("SystemCached should not be set for non-claude")
	}
	got := string(req.Tools[0].Schema)
	if got != `{"a":2,"b":1}` {
		t.Errorf("tools should still be normalized, got %s", got)
	}
}

func TestApply_PreservesNativeAnthropicShape(t *testing.T) {
	req := &ir.Request{
		OriginalProto:              "anthropic",
		AnthropicSystem:            []byte(`[{"type":"text","text":"native"}]`),
		System:                     string(make([]byte, 900)),
		Tools:                      []ir.ToolDef{{Name: "z", Schema: []byte(`{"b":1,"a":2}`)}, {Name: "a"}},
		AnthropicContextManagement: []byte(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`),
		AnthropicMetadata:          []byte(`{"user_id":"native"}`),
	}
	Apply(req, "claude")
	if req.SystemCached {
		t.Fatal("native Anthropic system should not get synthetic cache breakpoint")
	}
	if req.Tools[0].Name != "z" || string(req.Tools[0].Schema) != `{"b":1,"a":2}` {
		t.Fatalf("native Anthropic tools were modified: %#v", req.Tools)
	}
}
