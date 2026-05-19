// Package billing implements Claude Code billing attribution header injection
// and signing. Anthropic uses the x-anthropic-billing-header system text block
// to attribute requests to the correct billing bucket (Claude Code vs third-party).
//
// Missing or wrong cc_version → downgraded to third-party extra-usage quota.
// Wrong cch → request rejected or flagged as tampered.
//
// Reference: sub2api gateway_billing_header.go and gateway_billing_block.go.
package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

const (
	// CLIVersion is the version our gateway presents as.
	// Must match cliUserAgent in provider/claude/claude.go.
	CLIVersion = "2.1.138"

	// cchSeed is the same seed sub2api uses; must not change.
	cchSeed = uint64(0x6E52736AC806831E)
)

// fingerprintSalt matches the real Claude Code CLI and Parrot's FINGERPRINT_SALT.
const fingerprintSalt = "59cf53e54c78"

var (
	ccVersionRe      = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+(\.[0-9a-f]{3})?`)
	cchPlaceholderRe = regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)(00000)(;)`)
)

// InjectBillingBlock ensures the request body contains a correctly formed
// billing attribution block as the first system text block.
//
// If the body already has a billing block (client is Claude Code itself), we:
//  1. Sync cc_version to our CLIVersion.
//  2. Re-sign the cch field.
//
// If no billing block exists, we inject a minimal one so upstream attributes
// the request to Claude Code quota.
func InjectBillingBlock(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// If there's already a billing header, sync the version and re-sign.
	if hasBillingHeader(body) {
		body = syncVersion(body)
		body = signCCH(body)
		return body
	}
	// No billing block — inject one.
	body = injectBlock(body)
	body = signCCH(body)
	return body
}

func hasBillingHeader(body []byte) bool {
	return strings.Contains(string(body), "x-anthropic-billing-header")
}

// syncVersion rewrites cc_version=X.Y.Z in billing header text to CLIVersion.
func syncVersion(body []byte) []byte {
	type systemBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	fp := computeFingerprint(body, CLIVersion)
	versionWithFP := CLIVersion + "." + fp
	needle := "cc_version=" + versionWithFP
	if strings.Contains(string(body), needle) {
		return body
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	sysRaw, ok := raw["system"]
	if !ok {
		return body
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(sysRaw, &blocks); err != nil {
		return body
	}

	modified := false
	for i, blk := range blocks {
		var b systemBlock
		if err := json.Unmarshal(blk, &b); err != nil {
			continue
		}
		if b.Type == "text" && strings.HasPrefix(b.Text, "x-anthropic-billing-header") {
			newText := ccVersionRe.ReplaceAllString(b.Text, "cc_version="+versionWithFP)
			if newText != b.Text {
				b.Text = newText
				updated, err := json.Marshal(b)
				if err == nil {
					blocks[i] = updated
					modified = true
				}
			}
		}
	}
	if !modified {
		return body
	}
	newSys, err := json.Marshal(blocks)
	if err != nil {
		return body
	}
	raw["system"] = newSys
	result, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return result
}

// computeFingerprint replicates the real Claude Code CLI's cc_version suffix.
// Algorithm: SHA256(salt + firstUserText[4] + firstUserText[7] + firstUserText[20] + version)[:3]
// Reference: sub2api gateway_billing_block.go, Parrot cc_mimicry.py.
func computeFingerprint(body []byte, version string) string {
	firstText := extractFirstUserText(body)
	indices := []int{4, 7, 20}
	chars := make([]byte, 0, 3)
	for _, i := range indices {
		if i < len(firstText) {
			chars = append(chars, firstText[i])
		} else {
			chars = append(chars, '0')
		}
	}
	sum := sha256.Sum256([]byte(fingerprintSalt + string(chars) + version))
	return hex.EncodeToString(sum[:])[:3]
}

func extractFirstUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	var first string
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			first = content.String()
			return false
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					first = block.Get("text").String()
					return false
				}
				return true
			})
			return false
		}
		return false
	})
	return first
}

// injectBlock adds a minimal billing attribution block as the first system block.
func injectBlock(body []byte) []byte {
	fp := computeFingerprint(body, CLIVersion)
	billingText := fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=sdk-cli; cch=00000;",
		CLIVersion, fp,
	)
	claudeCodeText := "You are a Claude agent, built on Anthropic's Claude Agent SDK."

	block := map[string]any{
		"type": "text",
		"text": billingText,
	}
	block2 := map[string]any{
		"type":          "text",
		"text":          claudeCodeText,
		"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	var blocks []json.RawMessage
	if sysRaw, ok := raw["system"]; ok {
		switch {
		case strings.HasPrefix(strings.TrimSpace(string(sysRaw)), "["):
			_ = json.Unmarshal(sysRaw, &blocks)
		case strings.HasPrefix(strings.TrimSpace(string(sysRaw)), `"`):
			// String system prompt — wrap into a block.
			var s string
			if err := json.Unmarshal(sysRaw, &s); err == nil {
				b, _ := json.Marshal(map[string]any{"type": "text", "text": s})
				blocks = append(blocks, b)
			}
		}
	}

	b1, _ := json.Marshal(block)
	b2, _ := json.Marshal(block2)
	newBlocks := append([]json.RawMessage{b1, b2}, blocks...)

	newSys, err := json.Marshal(newBlocks)
	if err != nil {
		return body
	}
	raw["system"] = newSys

	result, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return result
}

// signCCH computes the xxHash64-seeded CCH and replaces the placeholder.
// cch = lower 20 bits of xxhash64(body, seed), formatted as 5 hex chars.
func signCCH(body []byte) []byte {
	if !cchPlaceholderRe.Match(body) {
		return body
	}
	h := xxhashSeeded(body, cchSeed)
	cch := fmt.Sprintf("%05x", h&0xFFFFF)
	return cchPlaceholderRe.ReplaceAll(body, []byte("${1}"+cch+"${3}"))
}

// xxhashSeeded computes xxHash64 with a custom seed.
func xxhashSeeded(data []byte, seed uint64) uint64 {
	d := xxhash.NewWithSeed(seed)
	_, _ = d.Write(data)
	return d.Sum64()
}

// ScrubInboundHeaders removes proxy/fingerprint headers from an outgoing
// upstream request that should NOT be forwarded (they reveal proxy infra).
// Reference: CPA ScrubProxyAndFingerprintHeaders.
func ScrubInboundHeaders(h interface{ Del(string) }) {
	for _, k := range []string{
		// Proxy tracing
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Forwarded-Port", "X-Real-IP", "Forwarded", "Via",
		// Client identity that reveals we're a proxy
		"X-Title", "Http-Referer", "Referer",
		// Electron / browser fingerprints not present in Node.js requests
		"Sec-Ch-Ua", "Sec-Ch-Ua-Mobile", "Sec-Ch-Ua-Platform",
		"Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-Dest",
		"Priority",
		// Accept-Encoding: Electron may add zstd which Node.js doesn't send
		"Accept-Encoding",
	} {
		h.Del(k)
	}
}

// ExtractCLIVersion parses "claude-cli/X.Y.Z ..." from User-Agent.
// Returns "" if not a claude-cli UA.
func ExtractCLIVersion(ua string) string {
	if !strings.HasPrefix(ua, "claude-cli/") {
		return ""
	}
	rest := ua[len("claude-cli/"):]
	// Take up to the next space or end-of-string.
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		end = len(rest)
	}
	ver := rest[:end]
	// Validate: must be X.Y.Z (optional .suffix).
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 3 {
		return ""
	}
	for _, p := range parts[:3] {
		base := strings.SplitN(p, "-", 2)[0]
		if _, err := strconv.Atoi(base); err != nil {
			return ""
		}
	}
	return ver
}
