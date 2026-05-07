package server

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/llm-pool/gateway/internal/config"
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
