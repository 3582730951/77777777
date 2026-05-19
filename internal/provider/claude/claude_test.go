package claude

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/billing"
	"github.com/llm-pool/gateway/internal/protocol/anthropic"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/tidwall/gjson"
)

func TestClaudeCodeMessagesHeadersMatchLatestCapture(t *testing.T) {
	h := http.Header{}
	setClaudeCodeMessagesHeaders(h, "access-token", "11111111-2222-4333-8444-555555555555")

	checks := map[string]string{
		"Authorization":               "Bearer access-token",
		"Accept":                      "application/json",
		"Content-Type":                "application/json",
		"User-Agent":                  "claude-cli/2.1.138 (external, sdk-cli)",
		"X-Claude-Code-Session-Id":    "11111111-2222-4333-8444-555555555555",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "0.93.0",
		"X-Stainless-OS":              stainlessOS(),
		"X-Stainless-Arch":            stainlessArch(),
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v24.3.0",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "600",
		"X-App":                       "cli",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"anthropic-version":                         "2023-06-01",
		"anthropic-beta":                            betaHeader,
	}
	for key, want := range checks {
		if got := h.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, stale := range []string{"fine-grained-tool-streaming", "effort-2025-11-24", "redact-thinking"} {
		if strings.Contains(h.Get("anthropic-beta"), stale) {
			t.Fatalf("stale beta %q still present: %s", stale, h.Get("anthropic-beta"))
		}
	}
	if !strings.Contains(h.Get("anthropic-beta"), "advisor-tool-2026-03-01") {
		t.Fatalf("latest advisor beta missing: %s", h.Get("anthropic-beta"))
	}
}

func TestBuildMessagesBodyMatchesClaudeCodeShape(t *testing.T) {
	req := &ir.Request{
		Model:        "claude-sonnet-4-6",
		System:       "system prompt",
		SystemCached: true,
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Parts: []ir.Part{{
				Kind: ir.PartText,
				Text: "say ok",
			}},
		}},
		Tools: []ir.ToolDef{{
			Name:            "bash",
			Description:     "run a shell command",
			Schema:          []byte(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`),
			CacheBreakpoint: true,
		}},
	}
	info := sessionInfo{AccountUUID: "acct-uuid"}

	body, err := buildMessagesBody(req, "claude-sonnet-4-6", info, "11111111-2222-4333-8444-555555555555", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(body) {
		t.Fatalf("invalid JSON: %s", body)
	}

	if got := gjson.GetBytes(body, "max_tokens").Int(); got != 32000 {
		t.Fatalf("max_tokens = %d", got)
	}
	if got := gjson.GetBytes(body, "context_management.edits.0.type").String(); got != "clear_thinking_20251015" {
		t.Fatalf("context_management edit type = %q", got)
	}
	if got := gjson.GetBytes(body, "context_management.edits.0.keep").String(); got != "all" {
		t.Fatalf("context_management keep = %q", got)
	}
	if got := gjson.GetBytes(body, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("system cache ttl = %q", got)
	}
	if got := gjson.GetBytes(body, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tool name = %q", got)
	}
	if got := gjson.GetBytes(body, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tool cache ttl = %q", got)
	}

	metadata := gjson.GetBytes(body, "metadata.user_id").String()
	var parsedMetadata map[string]string
	if err := json.Unmarshal([]byte(metadata), &parsedMetadata); err != nil {
		t.Fatalf("metadata.user_id is not JSON: %q", metadata)
	}
	if parsedMetadata["account_uuid"] != "acct-uuid" {
		t.Fatalf("account_uuid = %q", parsedMetadata["account_uuid"])
	}
	if parsedMetadata["session_id"] != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("session_id = %q", parsedMetadata["session_id"])
	}
	if len(parsedMetadata["device_id"]) != 64 {
		t.Fatalf("device_id length = %d", len(parsedMetadata["device_id"]))
	}
}

func TestInjectedBillingBlockUsesLatestClaudeCodeAgentAttribution(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "say ok"}},
		}},
	}
	body, err := buildMessagesBody(req, "claude-sonnet-4-6", sessionInfo{}, "11111111-2222-4333-8444-555555555555", "account-1")
	if err != nil {
		t.Fatal(err)
	}
	body = injectAndSignBilling(body)

	billingText := gjson.GetBytes(body, "system.0.text").String()
	if !strings.Contains(billingText, "cc_version="+billing.CLIVersion+".") {
		t.Fatalf("billing version not updated: %q", billingText)
	}
	if !strings.Contains(billingText, "cc_entrypoint=sdk-cli") {
		t.Fatalf("billing entrypoint not updated: %q", billingText)
	}
	if strings.Contains(billingText, "cch=00000") {
		t.Fatalf("billing cch was not signed: %q", billingText)
	}
	if got := gjson.GetBytes(body, "system.1.text").String(); got != "You are a Claude agent, built on Anthropic's Claude Agent SDK." {
		t.Fatalf("agent system text = %q", got)
	}
	if got := gjson.GetBytes(body, "system.1.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("agent system cache ttl = %q", got)
	}
}

func TestNewClaudeCodeSessionIDIsUUIDv4(t *testing.T) {
	id := newClaudeCodeSessionID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("session id is not uuid v4: %q", id)
	}
}

func TestClaudeCodeSessionIDUsesPreservedMetadataUserID(t *testing.T) {
	const sessionID = "11111111-2222-4333-8444-555555555555"
	req := &ir.Request{
		AnthropicMetadata:  []byte(`{"user_id":"{\"device_id\":\"dev\",\"account_uuid\":\"acct\",\"session_id\":\"` + sessionID + `\"}"}`),
		UpstreamSessionKey: "gateway-derived-key",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "say ok"}},
		}},
	}

	if got := claudeCodeSessionIDForRequest(req, "account-1"); got != sessionID {
		t.Fatalf("session id = %q, want preserved metadata session %q", got, sessionID)
	}
}

func TestClaudeCodeSessionIDUsesLegacyMetadataUserID(t *testing.T) {
	const sessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	req := &ir.Request{
		AnthropicMetadata:  []byte(`{"user_id":"user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account_acct_session_` + sessionID + `"}`),
		UpstreamSessionKey: "gateway-derived-key",
	}

	if got := claudeCodeSessionIDForRequest(req, "account-1"); got != sessionID {
		t.Fatalf("session id = %q, want legacy metadata session %q", got, sessionID)
	}
}

func TestClaudeCodeSessionIDDeterministicFromGatewaySessionKey(t *testing.T) {
	req := &ir.Request{
		UpstreamSessionKey: "tenant/group/api-key/client/first-user",
		Messages: []ir.Message{{
			Role:  ir.RoleUser,
			Parts: []ir.Part{{Kind: ir.PartText, Text: "say ok"}},
		}},
	}

	first := claudeCodeSessionIDForRequest(req, "account-1")
	second := claudeCodeSessionIDForRequest(req, "account-1")
	otherAccount := claudeCodeSessionIDForRequest(req, "account-2")

	if first != second {
		t.Fatalf("deterministic session changed: %q vs %q", first, second)
	}
	if first == otherAccount {
		t.Fatalf("different accounts should not share generated session id: %q", first)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("deterministic session id is not uuid v4: %q", first)
	}
}

func TestAnthropicDecodeBuildPreservesClaudeCodeDynamicShape(t *testing.T) {
	raw := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":32000,
		"stream":true,
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.138.abc; cc_entrypoint=sdk-cli; cch=00000;"},
			{"type":"text","text":"The following skills are available","cache_control":{"type":"ephemeral","ttl":"1h"}}
		],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"say ok","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"review"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"done"}],"is_error":true}]}
		],
		"tools":[
			{"name":"Skill","description":"execute a skill","input_schema":{"type":"object","properties":{"skill":{"type":"string"}}},"cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"name":"AskUserQuestion","description":"ask","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"Skill"},
		"metadata":{"user_id":"{\"device_id\":\"dev\",\"account_uuid\":\"acct\",\"session_id\":\"sess\"}"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"thinking":{"type":"adaptive","budget_tokens":1024}
	}`
	req, err := anthropic.Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, err := buildMessagesBody(req, "claude-sonnet-4-6", sessionInfo{AccountUUID: "gateway-acct"}, "gateway-session", "gateway-account")
	if err != nil {
		t.Fatal(err)
	}

	if got := gjson.GetBytes(body, "system.1.text").String(); got != "The following skills are available" {
		t.Fatalf("dynamic system block not preserved: %s", body)
	}
	if got := gjson.GetBytes(body, "system.1.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("system cache_control not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("message cache_control not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.0.name").String(); got != "Skill" {
		t.Fatalf("tool order/name not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tool cache_control not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "tool_choice.name").String(); got != "Skill" {
		t.Fatalf("tool_choice not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "metadata.user_id").String(); !strings.Contains(got, `"session_id":"sess"`) {
		t.Fatalf("metadata.user_id not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "context_management.edits.0.type").String(); got != "clear_thinking_20251015" {
		t.Fatalf("context_management not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking type not preserved: %q body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.2.content.0.is_error").Bool(); !got {
		t.Fatalf("tool_result is_error not preserved: body=%s", body)
	}
}
