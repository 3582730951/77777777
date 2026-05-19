package stealth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/tidwall/gjson"
)

func testIdentityRewriter() *IdentityRewriter {
	return NewIdentityRewriter(IdentityBundle{
		Username:                    "root",
		Hostname:                    "vps-alpha",
		Platform:                    "linux",
		OS:                          "linux",
		OSVersion:                   "6.1",
		Arch:                        "amd64",
		Shell:                       "/bin/bash",
		Terminal:                    "xterm-256color",
		Runtime:                     "node",
		RuntimeVersion:              "v24.3.0",
		AppVersion:                  "2.1.138",
		IPAddress:                   "10.0.0.10",
		ASN:                         "AS64512",
		Region:                      "us-east",
		Timezone:                    "UTC",
		DNSResolvers:                []string{"10.0.0.53", "1.1.1.1"},
		DNSProvider:                 "vps-dns",
		ProxyPresent:                false,
		ProxyType:                   "direct",
		HTTPProxy:                   "",
		HTTPSProxy:                  "",
		AllProxy:                    "",
		NoProxy:                     "",
		AnthropicBaseURL:            "https://api.anthropic.com",
		GatewayPresent:              false,
		GatewayName:                 "",
		CertStore:                   "system",
		ExtraCACertsPresent:         false,
		MTLSEnabled:                 false,
		WebFetchPreflightEnabled:    true,
		NonessentialTrafficDisabled: false,
		TelemetryDisabled:           false,
		ErrorReportingDisabled:      false,
		MCPProxyMode:                "direct",
		DirectConnectionPolicy:      "direct",
		StatsigStableID:             "stable_vps",
		StatsigUserID:               "statsig_user_vps",
		FeatureGateSeed:             "fgseed_vps",
		FeatureGates: map[string]bool{
			"claude_code_plan_mode":   true,
			"claude_code_multi_agent": false,
		},
		ExperimentGroups: map[string]string{
			"claude_code_ui": "variant_a",
			"tool_scheduler": "control",
		},
		UserID:           "user_vps",
		SessionID:        "sess_vps",
		AccountID:        "acct_vps",
		AccountUUID:      "00000000-0000-0000-0000-000000000001",
		OrganizationID:   "org_vps",
		OrganizationUUID: "00000000-0000-0000-0000-000000000002",
		Email:            "root@vps-alpha.local",
	}, IdentityAliases{
		Usernames: []string{"alice"},
		Hostnames: []string{"alice-mbp.local"},
		Emails:    []string{"alice@example.test"},
	})
}

func TestIdentityRewriterReplacesClaudeCodeContextFields(t *testing.T) {
	r := testIdentityRewriter()
	in := strings.Join([]string{
		"Current working directory: /Users/alice/private/app",
		"workspace.current_dir=/Users/alice/private/app",
		"transcript_path=/Users/alice/.claude/projects/-Users-alice-private-app.jsonl",
		"username: alice",
		"hostname: alice-mbp.local",
		"platform: darwin",
		"os.type: darwin",
		"host.arch: arm64",
		"terminal.type: iTerm.app",
		"app.version: 0.0.1",
		"runtime.version: v1.0.0",
		"ip_address: 192.0.2.44",
		"asn: AS65000",
		"region: local-test",
		"timezone: PST",
		"dns_resolvers: [8.8.8.8]",
		"proxy_present: false",
		"proxy_type: socks5",
		"http_proxy: http://user:pass@local-proxy.test:8080",
		"anthropic_base_url: http://localhost:8787",
		"gateway_present: false",
		"gateway_name: local-gateway",
		"cert_store: bundled",
		"extra_ca_certs_present: false",
		"mtls_enabled: false",
		"webfetch_preflight_enabled: false",
		"telemetry_disabled: true",
		"error_reporting_disabled: true",
		"mcp_proxy_mode: direct",
		"direct_connection_policy: direct",
		"statsig_stable_id: local-stable",
		"statsig_user_id: local-statsig-user",
		"feature_gate_seed: local-seed",
		"user.email: alice@example.test",
		"session_id: local-session",
		"user.id: local-user",
		"email alice@example.test",
	}, "\n")

	got := r.ReplaceString(in)

	for _, leaked := range []string{
		"alice-mbp.local", "username: alice", "darwin", "arm64", "iTerm.app",
		"app.version: 0.0.1", "v1.0.0", "192.0.2.44", "AS65000", "local-test",
		"PST", "8.8.8.8", "local-stable", "local-statsig-user",
		"local-seed", "local-session", "local-user", "alice@example.test",
		"user:pass", "local-proxy.test", "localhost:8787", "local-gateway",
		"cert_store: bundled",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("leaked %q after rewrite:\n%s", leaked, got)
		}
	}
	for _, want := range []string{
		"Current working directory: /Users/alice/private/app",
		"workspace.current_dir=/Users/alice/private/app",
		"transcript_path=/Users/alice/.claude/projects/-Users-alice-private-app.jsonl",
		"username: root", "hostname: vps-alpha", "os.type: linux",
		"host.arch: amd64", "terminal.type: xterm-256color",
		"app.version: 2.1.138", "runtime.version: v24.3.0",
		"ip_address: 10.0.0.10", "asn: AS64512", "region: us-east",
		"timezone: UTC", "dns_resolvers: 10.0.0.53,1.1.1.1",
		"proxy_present: false", "proxy_type: direct",
		"anthropic_base_url: https://api.anthropic.com",
		"gateway_present: false",
		"cert_store: system", "extra_ca_certs_present: false",
		"mtls_enabled: false", "webfetch_preflight_enabled: true",
		"telemetry_disabled: false", "error_reporting_disabled: false",
		"mcp_proxy_mode: direct",
		"direct_connection_policy: direct",
		"statsig_stable_id: stable_vps", "statsig_user_id: statsig_user_vps",
		"feature_gate_seed: fgseed_vps",
		"session_id: sess_vps", "user.id: user_vps",
		"root@vps-alpha.local",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q after rewrite:\n%s", want, got)
		}
	}
}

func TestIdentityRewriterRewritesJSONStringValuesOnly(t *testing.T) {
	r := testIdentityRewriter()
	body := []byte(`{"model":"gpt-5.5","metadata":{"keep":"ok"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"cwd=/Users/alice/private/app hostname=alice-mbp.local"}]}]}`)

	got := r.RewriteJSONBody(body)

	if !json.Valid(got) {
		t.Fatalf("rewritten body is invalid JSON: %s", got)
	}
	if bytes.Contains(got, []byte("alice-mbp.local")) {
		t.Fatalf("client identity leaked in JSON body: %s", got)
	}
	if gjson.GetBytes(got, "input.0.role").String() != "user" {
		t.Fatalf("JSON keys/role value should be preserved: %s", got)
	}
	text := gjson.GetBytes(got, "input.0.content.0.text").String()
	if !strings.Contains(text, "cwd=/Users/alice/private/app") || !strings.Contains(text, "hostname=vps-alpha") {
		t.Fatalf("identity values not rewritten in content: %q", text)
	}
}

func TestIdentityRewriterRewritesClaudeCodeJSONKeys(t *testing.T) {
	r := testIdentityRewriter()
	body := []byte(`{"session_id":"local-session","user":{"id":"local-user","email":"alice@example.test","account_uuid":"local-account"},"organization":{"id":"local-org"},"cwd":"/Users/alice/private/app","transcript_path":"/Users/alice/.claude/projects/x.jsonl","workspace":{"host_paths":["/Users/alice/private/app","/Users/alice/other"]},"terminal":{"type":"iTerm.app"},"app":{"version":"0.0.1"},"runtime":{"version":"v1.0.0"},"platform":"darwin","os":{"type":"darwin","version":"14.0"},"host":{"arch":"arm64","name":"alice-mbp.local"},"ip_address":"192.0.2.44","asn":"AS65000","region":"local-test","timezone":"PST","dns":{"provider":"google","resolvers":["8.8.8.8"]},"proxy":{"present":false,"type":"socks5"},"http_proxy":"http://user:pass@local-proxy.test:8080","https_proxy":"http://local-proxy.test:8443","no_proxy":"localhost","anthropic_base_url":"http://localhost:8787","gateway":{"present":false,"name":"local-gateway"},"cert_store":"bundled","extra_ca_certs_present":false,"mtls":{"enabled":false},"webfetch":{"preflight_enabled":false},"telemetry_disabled":true,"error_reporting_disabled":true,"mcp_proxy_mode":"direct","direct_connection_policy":"direct","statsig":{"stable_id":"local-stable","user_id":"local-statsig-user","feature_gates":{"claude_code_plan_mode":false},"experiment_groups":{"claude_code_ui":"control"}},"feature":{"gate":{"seed":"local-seed"}}}`)

	got := r.RewriteJSONBody(body)

	for _, leaked := range []string{
		"local-session", "local-user", "local-account", "local-org",
		"alice-mbp.local", "iTerm.app", `"darwin"`, `"arm64"`,
		"alice@example.test", `"version":"0.0.1"`, "v1.0.0", "192.0.2.44",
		"AS65000", "local-test", "PST", "8.8.8.8", "google",
		"local-stable", "local-statsig-user", "local-seed",
		"user:pass", "local-proxy.test", "localhost:8787", "local-gateway",
		`"type":"socks5"`, `"cert_store":"bundled"`,
	} {
		if strings.Contains(string(got), leaked) {
			t.Fatalf("leaked %q after key-aware JSON rewrite: %s", leaked, got)
		}
	}
	checks := map[string]string{
		"session_id":               "sess_vps",
		"user.id":                  "user_vps",
		"user.email":               "root@vps-alpha.local",
		"user.account_uuid":        "00000000-0000-0000-0000-000000000001",
		"organization.id":          "org_vps",
		"cwd":                      "/Users/alice/private/app",
		"transcript_path":          "/Users/alice/.claude/projects/x.jsonl",
		"terminal.type":            "xterm-256color",
		"app.version":              "2.1.138",
		"runtime.version":          "v24.3.0",
		"platform":                 "linux",
		"os.type":                  "linux",
		"os.version":               "6.1",
		"host.arch":                "amd64",
		"host.name":                "vps-alpha",
		"ip_address":               "10.0.0.10",
		"asn":                      "AS64512",
		"region":                   "us-east",
		"timezone":                 "UTC",
		"dns.provider":             "vps-dns",
		"statsig.stable_id":        "stable_vps",
		"statsig.user_id":          "statsig_user_vps",
		"feature.gate.seed":        "fgseed_vps",
		"proxy.type":               "direct",
		"http_proxy":               "",
		"https_proxy":              "",
		"no_proxy":                 "",
		"anthropic_base_url":       "https://api.anthropic.com",
		"gateway.name":             "",
		"cert_store":               "system",
		"mcp_proxy_mode":           "direct",
		"direct_connection_policy": "direct",
	}
	for path, want := range checks {
		if gotVal := gjson.GetBytes(got, path).String(); gotVal != want {
			t.Fatalf("%s = %q, want %q body=%s", path, gotVal, want, got)
		}
	}
	if gotPath := gjson.GetBytes(got, "workspace.host_paths.0").String(); gotPath != "/Users/alice/private/app" {
		t.Fatalf("workspace.host_paths.0 = %q body=%s", gotPath, got)
	}
	if gotPath := gjson.GetBytes(got, "workspace.host_paths.1").String(); gotPath != "/Users/alice/other" {
		t.Fatalf("workspace.host_paths.1 = %q body=%s", gotPath, got)
	}
	if gotResolver := gjson.GetBytes(got, "dns.resolvers.0").String(); gotResolver != "10.0.0.53" {
		t.Fatalf("dns.resolvers.0 = %q body=%s", gotResolver, got)
	}
	if gotResolver := gjson.GetBytes(got, "dns.resolvers.1").String(); gotResolver != "1.1.1.1" {
		t.Fatalf("dns.resolvers.1 = %q body=%s", gotResolver, got)
	}
	if gotGate := gjson.GetBytes(got, "statsig.feature_gates.claude_code_plan_mode").Bool(); !gotGate {
		t.Fatalf("feature gate not rewritten: %s", got)
	}
	if gotGroup := gjson.GetBytes(got, "statsig.experiment_groups.claude_code_ui").String(); gotGroup != "variant_a" {
		t.Fatalf("experiment group = %q body=%s", gotGroup, got)
	}
}

func TestLoadOrCreateIdentityBundlePersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")

	first, err := LoadOrCreateIdentityBundle(path)
	if err != nil {
		t.Fatalf("first load/create: %v", err)
	}
	if first.Username == "" || first.SessionID == "" {
		t.Fatalf("generated identity incomplete: %+v", first)
	}

	var stored persistedIdentity
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted identity: %v", err)
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("persisted identity invalid: %v\n%s", err, b)
	}
	stored.Bundle.Username = "persisted-user"
	next, _ := json.Marshal(stored)
	if err := os.WriteFile(path, next, 0600); err != nil {
		t.Fatalf("rewrite persisted identity fixture: %v", err)
	}

	second, err := LoadOrCreateIdentityBundle(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.Username != "persisted-user" {
		t.Fatalf("restart should reuse persisted identity, got %+v", second)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("restart regenerated session_id: first=%s second=%s", first.SessionID, second.SessionID)
	}
}

func TestLoadOrCreateAccountIdentityBundlePersistsPerAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	base := IdentityBundle{
		Username:     "root",
		Hostname:     "vps-alpha",
		Platform:     "linux",
		OS:           "linux",
		Arch:         "amd64",
		IPAddress:    "10.0.0.10",
		ASN:          "AS64512",
		Region:       "us-east",
		Timezone:     "UTC",
		DNSResolvers: []string{"10.0.0.53"},
		DNSProvider:  "vps-dns",
	}
	if err := writeIdentityBundleOnce(path, normalizeIdentityBundle(base)); err != nil {
		t.Fatalf("write base identity: %v", err)
	}

	first, err := LoadOrCreateAccountIdentityBundle(path, "claude", "acc-1", "one@example.test")
	if err != nil {
		t.Fatalf("first account identity: %v", err)
	}
	second, err := LoadOrCreateAccountIdentityBundle(path, "claude", "acc-2", "two@example.test")
	if err != nil {
		t.Fatalf("second account identity: %v", err)
	}
	again, err := LoadOrCreateAccountIdentityBundle(path, "claude", "acc-1", "one@example.test")
	if err != nil {
		t.Fatalf("reload first account identity: %v", err)
	}

	if first.SessionID == second.SessionID || first.StatsigStableID == second.StatsigStableID {
		t.Fatalf("different accounts should not share account-bound ids: first=%+v second=%+v", first, second)
	}
	if first.SessionID != again.SessionID || first.StatsigStableID != again.StatsigStableID || first.FeatureGateSeed != again.FeatureGateSeed {
		t.Fatalf("same account did not reuse persisted identity: first=%+v again=%+v", first, again)
	}
	if first.IPAddress != "10.0.0.10" || strings.Join(first.DNSResolvers, ",") != "10.0.0.53" {
		t.Fatalf("account identity should retain VPS network base: %+v", first)
	}
	if len(first.FeatureGates) == 0 || len(first.ExperimentGroups) == 0 {
		t.Fatalf("account identity missing feature-gate state: %+v", first)
	}
}

func TestIdentityRewriterRewritesIRRequest(t *testing.T) {
	r := testIdentityRewriter()
	req := &ir.Request{
		System:                     "home=/Users/alice",
		AnthropicSystemText:        "Current working directory: /Users/alice/private/app",
		AnthropicSystem:            []byte(`[{"type":"text","text":"Current working directory: /Users/alice/private/app\nhostname: alice-mbp.local"}]`),
		AnthropicMetadata:          []byte(`{"cwd":"/Users/alice/private/app","user_id":"local-user"}`),
		AnthropicContextManagement: []byte(`{"workspace":{"host_paths":["/Users/alice/private/app"]}}`),
		AnthropicToolChoice:        []byte(`{"type":"tool","name":"shell","cwd":"/Users/alice/private/app"}`),
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Parts: []ir.Part{
				{Kind: ir.PartText, Text: "Run tests in /Users/alice/private/app as alice"},
				{Kind: ir.PartToolUse, ToolUseInput: []byte(`{"cmd":"cd /Users/alice/private/app && pwd","user":"alice"}`)},
				{Kind: ir.PartToolResult, ToolResultBytes: []byte(`{"cwd":"/Users/alice/private/app","hostname":"alice-mbp.local"}`)},
			},
		}},
		Tools: []ir.ToolDef{{
			Name:        "shell",
			Description: "Runs commands for alice in /Users/alice/private/app",
			Schema:      []byte(`{"type":"object","properties":{"path":{"default":"/Users/alice/private/app"}}}`),
		}},
	}

	r.RewriteIRRequest(req)

	all := req.System + req.Messages[0].Parts[0].Text + string(req.Messages[0].Parts[1].ToolUseInput) +
		string(req.Messages[0].Parts[2].ToolResultBytes) + req.Tools[0].Description + string(req.Tools[0].Schema) +
		req.AnthropicSystemText + string(req.AnthropicSystem) + string(req.AnthropicMetadata) +
		string(req.AnthropicContextManagement) + string(req.AnthropicToolChoice)
	if strings.Contains(all, "alice-mbp.local") || strings.Contains(all, "alice@example.test") {
		t.Fatalf("client identity leaked after IR rewrite: %s", all)
	}
	if !strings.Contains(all, "/Users/alice/private/app") {
		t.Fatalf("project paths should be preserved after IR rewrite: %s", all)
	}
	if req.Tools[0].Name != "shell" {
		t.Fatalf("tool name must not be rewritten: %q", req.Tools[0].Name)
	}
}
