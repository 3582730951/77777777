package server

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/tidwall/gjson"
)

func TestOptimizeResponsesToolOutputsCompressesLargeFunctionOutput(t *testing.T) {
	var out strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&out, "line %03d: normal output\n", i)
	}
	out.WriteString("ERROR: build failed\n")
	for i := 300; i < 600; i++ {
		fmt.Fprintf(&out, "line %03d: normal output\n", i)
	}
	body := fmt.Sprintf(`{"model":"gpt-5.5","instructions":"keep","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"do not alter"}]},{"type":"function_call_output","call_id":"call_1","output":%q}],"stream":true}`, out.String())

	got := optimizeResponsesToolOutputs([]byte(body), config.TokenOptimizer{
		Mode:                        "safe",
		MinToolOutputBytes:          1024,
		MaxOptimizedToolOutputBytes: 4096,
		HeadLines:                   8,
		TailLines:                   8,
		ErrorContextLines:           2,
	})

	if string(got) == body {
		t.Fatal("expected optimized body")
	}
	if inst := gjson.GetBytes(got, "instructions").String(); inst != "keep" {
		t.Fatalf("instructions changed: %q", inst)
	}
	if user := gjson.GetBytes(got, "input.0.content.0.text").String(); user != "do not alter" {
		t.Fatalf("user input changed: %q", user)
	}
	optimized := gjson.GetBytes(got, "input.1.output").String()
	if !strings.Contains(optimized, "ERROR: build failed") {
		t.Fatalf("important error context was lost: %s", optimized)
	}
	if !strings.Contains(optimized, "gateway token optimizer") {
		t.Fatalf("optimization marker missing: %s", optimized)
	}
	if len(optimized) >= len(out.String()) {
		t.Fatalf("optimized output should be shorter: got %d original %d", len(optimized), len(out.String()))
	}
}

func TestOptimizeResponsesToolOutputsSkipsStructuredJSONInSafeMode(t *testing.T) {
	output := `{"items":[{"id":1,"value":"keep exact"}]}`
	body := fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"call_1","output":%q}]}`, output)

	got := optimizeResponsesToolOutputs([]byte(body), config.TokenOptimizer{
		Mode:               "safe",
		MinToolOutputBytes: 1,
	})

	if string(got) != body {
		t.Fatalf("safe mode should not rewrite structured json output: %s", got)
	}
}

func TestOptimizeResponsesToolOutputsOff(t *testing.T) {
	body := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"large but disabled"}]}`
	got := optimizeResponsesToolOutputs([]byte(body), config.TokenOptimizer{
		Mode:               "off",
		MinToolOutputBytes: 1,
	})
	if string(got) != body {
		t.Fatalf("off mode should not rewrite body: %s", got)
	}
}

func TestOptimizeResponsesToolOutputsDefaultOff(t *testing.T) {
	output := strings.Repeat("same noisy line\n", 200)
	body := fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"call_1","output":%q}]}`, output)

	got := optimizeResponsesToolOutputs([]byte(body), config.TokenOptimizer{
		MinToolOutputBytes: 1,
	})

	if string(got) != body {
		t.Fatalf("default optimizer mode should preserve direct Codex bodies: %s", got)
	}
}

func TestOptimizeResponsesToolOutputsChatGPTOnly(t *testing.T) {
	output := strings.Repeat("same noisy line\n", 200)
	body := fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"call_1","output":%q}]}`, output)

	got := optimizeResponsesToolOutputsForProvider([]byte(body), config.TokenOptimizer{
		Mode:               "safe",
		MinToolOutputBytes: 1,
	}, "claude")

	if string(got) != body {
		t.Fatalf("non-ChatGPT providers should not be optimized: %s", got)
	}
}

func TestOptimizeResponsesToolOutputsCleanupModeOnlyCleansFunctionOutput(t *testing.T) {
	output := "\x1b[31mRunning\x1b[0m 10%\rRunning 20%\rRunning done\n\n\n\nok\n"
	body := fmt.Sprintf(`{"model":"gpt-5.5","instructions":"keep","reasoning":{"effort":"xhigh"},"max_output_tokens":4096,"service_tier":"priority","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"do not alter"}]},{"type":"function_call_output","call_id":"call_1","output":%q}],"stream":true}`, output)

	got := optimizeResponsesToolOutputs([]byte(body), config.TokenOptimizer{
		Mode:               "cleanup",
		MinToolOutputBytes: 1,
	})

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.5" {
		t.Fatalf("model changed: %q", model)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning effort changed: %q", effort)
	}
	if max := gjson.GetBytes(got, "max_output_tokens").Int(); max != 4096 {
		t.Fatalf("max_output_tokens changed: %d", max)
	}
	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier changed: %q", tier)
	}
	if inst := gjson.GetBytes(got, "instructions").String(); inst != "keep" {
		t.Fatalf("instructions changed: %q", inst)
	}
	if user := gjson.GetBytes(got, "input.0.content.0.text").String(); user != "do not alter" {
		t.Fatalf("user input changed: %q", user)
	}
	optimized := gjson.GetBytes(got, "input.1.output").String()
	if strings.Contains(optimized, "\x1b[") || strings.Contains(optimized, "\r") {
		t.Fatalf("terminal noise was not removed from function output: %q", optimized)
	}
}

func TestOptimizeResponsesToolOutputsNeverOptimizesGroupSystemPrompt(t *testing.T) {
	var out strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&out, "=== RUN   TestNoisy%03d\n--- PASS: TestNoisy%03d (0.00s)\n", i, i)
	}
	out.WriteString("--- FAIL: TestImportant (0.01s)\n    main_test.go:12: assertion failed\n")
	for i := 300; i < 600; i++ {
		fmt.Fprintf(&out, "=== RUN   TestNoisy%03d\n--- PASS: TestNoisy%03d (0.00s)\n", i, i)
	}
	groupPrompt := "\x1b[31mKEEP ANSI IN GROUP PROMPT\x1b[0m\n" + strings.Repeat("policy line\n", 50)
	body := fmt.Sprintf(`{"model":"gpt-5.5","instructions":"codex-base","input":[{"type":"function_call_output","call_id":"call_1","output":%q}]}`, out.String())
	bodyWithPrompt := applyGroupSystemPromptToResponsesBody([]byte(body), &domain.Group{
		SystemPrompt: groupPrompt,
	})

	got := optimizeResponsesToolOutputs(bodyWithPrompt, config.TokenOptimizer{
		Mode:                        "guarded",
		MinToolOutputBytes:          1,
		MaxOptimizedToolOutputBytes: 2048,
		HeadLines:                   4,
		TailLines:                   4,
		ErrorContextLines:           1,
	})

	if inst := gjson.GetBytes(got, "instructions").String(); inst != groupPrompt+"\n\ncodex-base" {
		t.Fatalf("group system prompt must be byte-for-byte preserved in instructions:\n%q", inst)
	}
	optimized := gjson.GetBytes(got, "input.0.output").String()
	if !strings.Contains(optimized, "TestImportant") || !strings.Contains(optimized, "gateway token optimizer") {
		t.Fatalf("expected only function_call_output to be optimized, got: %s", optimized)
	}
}

func TestShrinkOptimizedOutputKeepsUTF8Valid(t *testing.T) {
	input := strings.Repeat("前缀内容", 80) + "\nERROR: 构建失败\n" + strings.Repeat("尾部内容", 80)
	got := shrinkOptimizedOutput(input, 257)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output must remain valid utf-8: %q", got)
	}
	if !strings.Contains(got, "gateway token optimizer") {
		t.Fatalf("truncation marker missing: %s", got)
	}
}
