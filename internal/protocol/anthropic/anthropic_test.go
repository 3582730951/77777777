package anthropic

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
)

func TestEncoderStreamsToolUseForClaudeCode(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewEncoder(rec, "claude-sonnet-4-6", true)
	enc.WriteHeaders()

	events := make(chan ir.Event, 4)
	events <- ir.Event{Kind: ir.EvToolUseStart, ToolID: "toolu_1", ToolName: "Skill"}
	events <- ir.Event{Kind: ir.EvToolUseDelta, ToolDelta: []byte(`{"skill":"review"}`)}
	events <- ir.Event{Kind: ir.EvToolUseEnd}
	events <- ir.Event{Kind: ir.EvDone, FinishReason: "tool_use"}
	close(events)

	if err := enc.Stream(events); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: content_block_start",
		`"type":"tool_use"`,
		`"id":"toolu_1"`,
		`"name":"Skill"`,
		"event: content_block_delta",
		`"type":"input_json_delta"`,
		`"partial_json":"{\"skill\":\"review\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in SSE body:\n%s", want, body)
		}
	}
}
