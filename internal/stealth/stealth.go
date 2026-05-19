// Package stealth centralises provider protocol hygiene for all three providers.
// L0: Header completeness. L1: Billing attribution (see billing/).
// L2: Error classification. L3: Request pacing. L4: CF detection.
package stealth

import (
	"math"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// ClaudeCodeBetaHeader is the complete beta list for Claude Code CLI 2.1.138.
// ALL betas must be present — missing any causes Anthropic to bill against
// third-party extra-usage instead of the Claude Code subscription quota.
const ClaudeCodeBetaHeader = "claude-code-20250219,oauth-2025-04-20," +
	"interleaved-thinking-2025-05-14,context-management-2025-06-27," +
	"prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01," +
	"extended-cache-ttl-2025-04-11"

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

var proxyDisclosureHeaders = []string{
	"Client-Ip",
	"X-Forwarded-For",
	"X-Forwarded",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Forwarded-Protocol",
	"X-Forwarded-Port",
	"X-Forwarded-Scheme",
	"X-Forwarded-Ssl",
	"X-Forwarded-Server",
	"X-Forwarded-Uri",
	"X-Forwarded-Path",
	"X-Forwarded-Prefix",
	"X-Forwarded-Method",
	"X-Real-Ip",
	"X-Client-Ip",
	"X-Originating-Ip",
	"X-Remote-Ip",
	"X-Remote-Addr",
	"Forwarded",
	"Via",
	"True-Client-Ip",
	"Cf-Connecting-Ip",
	"Fastly-Client-Ip",
	"X-Cluster-Client-Ip",
	"X-Original-Forwarded-For",
	"X-Original-Host",
	"X-Original-Url",
	"X-Original-Uri",
	"X-Original-Method",
	"X-Rewrite-Url",
	"X-Scheme",
	"X-Url-Scheme",
	"X-Envoy-External-Address",
	"X-Envoy-Internal",
	"X-Envoy-Original-Path",
	"X-Envoy-Decorator-Operation",
	"X-Envoy-Attempt-Count",
	"X-Envoy-Expected-Rq-Timeout-Ms",
	"X-Envoy-Upstream-Service-Time",
	"X-Nginx-Proxy",
	"X-Proxy-Cache",
	"X-Varnish",
	"X-Served-By",
	"X-Cache",
	"X-Cache-Hits",
	"X-Timer",
	"X-Request-Start",
	"X-Queue-Start",
}

var proxyDisclosureHeaderPrefixes = []string{
	"cf-aig-",
	"helicone-",
	"langfuse-",
	"langsmith-",
	"x-ai-gateway-",
	"x-bt-",
	"x-gateway-",
	"x-kong-",
	"x-langfuse-",
	"x-langsmith-",
	"x-litellm-",
	"x-llm-",
	"x-openrouter-",
	"x-portkey-",
}

var tracingDisclosureHeaders = []string{
	"X-Request-Id",
	"X-Correlation-Id",
	"X-Trace-Id",
	"X-Amzn-Trace-Id",
	"X-Cloud-Trace-Context",
	"X-B3-Traceid",
	"X-B3-Spanid",
	"X-B3-Parentspanid",
	"X-B3-Sampled",
	"X-B3-Flags",
	"Traceparent",
	"Tracestate",
	"Baggage",
	"Uber-Trace-Id",
	"X-Datadog-Trace-Id",
	"X-Datadog-Parent-Id",
	"X-Datadog-Sampling-Priority",
	"X-Datadog-Origin",
}

var browserOnlyHeaders = []string{
	"X-Title",
	"Http-Referer",
	"Referer",
	"Origin",
	"Sec-Ch-Ua",
	"Sec-Ch-Ua-Mobile",
	"Sec-Ch-Ua-Platform",
	"Sec-Ch-Ua-Full-Version-List",
	"Sec-Ch-Ua-Arch",
	"Sec-Ch-Ua-Bitness",
	"Sec-Ch-Ua-Model",
	"Sec-Ch-Ua-Wow64",
	"Sec-Fetch-Mode",
	"Sec-Fetch-Site",
	"Sec-Fetch-Dest",
	"Sec-Fetch-User",
	"Priority",
	"Dnt",
	"Upgrade-Insecure-Requests",
}

var clientFrameworkHeaders = []string{
	"X-Client-Info",
	"X-Client-Version",
	"X-Sdk-Name",
	"X-Sdk-Version",
}

// ScrubProxy removes hop-by-hop, proxy disclosure, tracing, and browser-only
// headers before requests leave the gateway boundary. It does not change
// Authorization, model, context, tools, reasoning, or token budget fields.
func ScrubProxy(h http.Header) {
	if h == nil {
		return
	}
	for _, token := range connectionHeaderTokens(h) {
		h.Del(token)
	}
	for _, group := range [][]string{
		hopByHopHeaders,
		proxyDisclosureHeaders,
		tracingDisclosureHeaders,
		browserOnlyHeaders,
		clientFrameworkHeaders,
	} {
		for _, k := range group {
			h.Del(k)
		}
	}
	delHeaderPrefixes(h, proxyDisclosureHeaderPrefixes)
}

func connectionHeaderTokens(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			token := strings.TrimSpace(part)
			if token != "" {
				out = append(out, token)
			}
		}
	}
	return out
}

func delHeaderPrefixes(h http.Header, prefixes []string) {
	for key := range h {
		lower := strings.ToLower(key)
		for _, prefix := range prefixes {
			if strings.HasPrefix(lower, prefix) {
				h.Del(key)
				break
			}
		}
	}
}

// CodexHeaders sets the full Codex CLI header set for chatgpt.com requests.
func CodexHeaders(h http.Header, ua, accountID, sessionID, accessToken string) {
	if ua == "" {
		ua = "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
	}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", ua)
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("originator", "codex_cli_rs")
	h.Set("ChatGPT-Account-Id", accountID)
	h.Set("session_id", sessionID)
	h.Set("Accept-Encoding", "gzip, deflate, br")
	ScrubProxy(h)
}

// ClaudeCodeHeaders sets the full Claude Code CLI 2.1.138 header set.
func ClaudeCodeHeaders(h http.Header, accessToken string) {
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("anthropic-beta", ClaudeCodeBetaHeader)
	h.Set("User-Agent", "claude-cli/2.1.138 (external, sdk-cli)")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Package-Version", "0.93.0")
	h.Set("X-Stainless-OS", stealthStainlessOS())
	h.Set("X-Stainless-Arch", stealthStainlessArch())
	h.Set("X-Stainless-Runtime", "node")
	h.Set("X-Stainless-Runtime-Version", "v24.3.0")
	h.Set("X-Stainless-Retry-Count", "0")
	h.Set("X-Stainless-Timeout", "600")
	h.Set("X-App", "cli")
	h.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	h.Set("Accept-Encoding", "gzip, deflate, br")
	ScrubProxy(h)
}

func stealthStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	default:
		return "Unknown"
	}
}

func stealthStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x32"
	default:
		return "unknown"
	}
}

// GeminiCLIHeaders sets Gemini CLI 0.1.5 headers for cloudcode-pa.googleapis.com.
func GeminiCLIHeaders(h http.Header, accessToken string) {
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "GeminiCLI/0.1.5 (Windows; AMD64)")
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip, deflate, br")
	h.Set("X-Goog-Api-Client", "google-genai-sdk/1.41.0 gl-node/v22.19.0")
	ScrubProxy(h)
}

// IsCFChallenge returns true when upstream response is a Cloudflare block.
func IsCFChallenge(status int, body []byte, h http.Header) bool {
	if h.Get("cf-mitigated") != "" || h.Get("cf-ray") != "" {
		lower := strings.ToLower(string(body))
		for _, kw := range []string{"cloudflare", "challenge", "captcha", "turnstile", "just a moment"} {
			if strings.Contains(lower, kw) {
				return true
			}
		}
		if status == 403 || status == 503 {
			return true
		}
	}
	return false
}

// IsQuotaError returns true for 429 rate-limit responses.
func IsQuotaError(status int, body []byte) bool {
	if status != 429 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, kw := range []string{"rate limit", "quota", "limit exceeded", "too many requests", "message cap"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return true
}

// RequestJitter returns a human-like delay using Pareto distribution.
// Real users have mostly short intervals (typing) with occasional long
// pauses (thinking/reading). Pareto distribution captures this pattern
// much better than uniform jitter.
//
// Distribution: P(x) = alpha * x_min^alpha / x^(alpha+1)
//
//	alpha=1.5 gives 80/20 shape: 80% of delays < 2*base, 20% are 2-10x longer
func RequestJitter(maxRPS float64, jitterPct int) time.Duration {
	if maxRPS <= 0 {
		return 0
	}
	base := float64(time.Second) / maxRPS

	// Pareto distribution: x = x_min / u^(1/alpha) where u ~ Uniform(0,1)
	// alpha=1.5: mostly short, occasional long pause (mimics human think time)
	alpha := 1.5
	u := rand.Float64()
	if u < 0.001 {
		u = 0.001 // avoid division by zero
	}
	pareto := base / math.Pow(u, 1.0/alpha)

	// Cap at 10x base to avoid extremely long waits
	maxDelay := base * 10
	if pareto > maxDelay {
		pareto = maxDelay
	}

	// Add micro-jitter (±5%) to prevent exact interval patterns
	microJitter := pareto * 0.05 * (rand.Float64()*2 - 1)
	result := time.Duration(pareto + microJitter)

	if result < 30*time.Millisecond {
		return 30 * time.Millisecond
	}
	return result
}
