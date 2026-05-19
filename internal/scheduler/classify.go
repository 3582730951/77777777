package scheduler

import (
	"errors"
	"strings"

	"github.com/llm-pool/gateway/internal/domain"
)

// ClassifyError inspects an upstream-side error / status to decide which
// ErrorClass it belongs to. Used by the gateway after a failed Invoke to
// apply MarkFailure with the right semantics.
func ClassifyError(httpStatus int, body string, err error) domain.ErrorClass {
	if err != nil {
		s := err.Error()
		sLower := strings.ToLower(s)
		if strings.Contains(sLower, "account banned") || isBanSignal(sLower) {
			return domain.ErrBanned
		}
		if isCapacitySignal(sLower) {
			return domain.ErrRateLimited
		}
		if strings.Contains(sLower, "upstream quota:") {
			return domain.ErrQuotaExhausted
		}
		var nerr interface{ Timeout() bool }
		if errors.As(err, &nerr) && nerr.Timeout() {
			return domain.ErrNetwork
		}
		if strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") ||
			strings.Contains(s, "i/o timeout") || strings.Contains(s, "tls:") {
			return domain.ErrNetwork
		}
	}
	bodyLower := strings.ToLower(body)

	// Ban/deactivation signals (any status code)
	if isBanSignal(bodyLower) {
		return domain.ErrBanned
	}
	if isUsageLimitSignal(bodyLower) {
		return domain.ErrQuotaExhausted
	}
	if isCapacitySignal(bodyLower) {
		return domain.ErrRateLimited
	}

	switch httpStatus {
	case 401, 403:
		if strings.Contains(bodyLower, "cloudflare") || strings.Contains(bodyLower, "cf-mitigated") ||
			strings.Contains(bodyLower, "challenge") || strings.Contains(bodyLower, "turnstile") {
			return domain.ErrCFChallenge
		}
		return domain.ErrAuthFailed
	case 429:
		if strings.Contains(bodyLower, "quota") || strings.Contains(bodyLower, "exceeded") ||
			strings.Contains(bodyLower, "limit reached") || strings.Contains(bodyLower, "you've reached") {
			return domain.ErrQuotaExhausted
		}
		return domain.ErrRateLimited
	case 503:
		if strings.Contains(bodyLower, "cloudflare") {
			return domain.ErrCFChallenge
		}
		return domain.ErrUpstreamError
	}
	if httpStatus >= 500 {
		return domain.ErrUpstreamError
	}
	if httpStatus >= 400 {
		return domain.ErrUpstreamError
	}
	return domain.ErrUnknown
}

func isUsageLimitSignal(s string) bool {
	return strings.Contains(s, "you've hit your usage limit") ||
		strings.Contains(s, "you have hit your usage limit") ||
		strings.Contains(s, "usage limit has been reached") ||
		strings.Contains(s, "insufficient_quota") ||
		strings.Contains(s, "quota exceeded") ||
		strings.Contains(s, "usage exhausted") ||
		(strings.Contains(s, "usage limit") && strings.Contains(s, "try again"))
}

func isCapacitySignal(s string) bool {
	return strings.Contains(s, "selected model is at capacity") ||
		strings.Contains(s, "model is at capacity") ||
		strings.Contains(s, "please try a different model") ||
		strings.Contains(s, "rate_limit_error")
}

// isBanSignal detects account deactivation/ban signals in response bodies.
// Patterns sourced from Codex-Manager isBannedAccount() and ChatGPT/Claude error formats.
func isBanSignal(bodyLower string) bool {
	return strings.Contains(bodyLower, "account_deactivated") ||
		strings.Contains(bodyLower, "workspace_deactivated") ||
		strings.Contains(bodyLower, "deactivated_workspace") ||
		strings.Contains(bodyLower, "workspace deactivated") ||
		strings.Contains(bodyLower, "workspace-deactivated") ||
		strings.Contains(bodyLower, "deactivated workspace") ||
		strings.Contains(bodyLower, "account deactivated") ||
		strings.Contains(bodyLower, "team_deactivated") ||
		strings.Contains(bodyLower, "organization_deactivated") ||
		strings.Contains(bodyLower, "org_deactivated") ||
		strings.Contains(bodyLower, "user_deactivated") ||
		strings.Contains(bodyLower, "account has been deactivated") ||
		strings.Contains(bodyLower, "account has been suspended") ||
		strings.Contains(bodyLower, "account has been banned") ||
		strings.Contains(bodyLower, "your account has been deactivated") ||
		strings.Contains(bodyLower, "your account has been suspended") ||
		strings.Contains(bodyLower, "your account has been disabled") ||
		strings.Contains(bodyLower, "account is disabled") ||
		strings.Contains(bodyLower, "account is suspended") ||
		strings.Contains(bodyLower, "account_suspended") ||
		strings.Contains(bodyLower, "your account has been flagged") ||
		strings.Contains(bodyLower, "account_banned") ||
		strings.Contains(bodyLower, "account_disabled") ||
		strings.Contains(bodyLower, "workspace disabled") ||
		strings.Contains(bodyLower, "workspace_disabled") ||
		strings.Contains(bodyLower, "organization has been disabled") ||
		strings.Contains(bodyLower, "organization_disabled") ||
		strings.Contains(bodyLower, "org_disabled") ||
		strings.Contains(bodyLower, "your access was terminated") ||
		strings.Contains(bodyLower, "account was terminated") ||
		strings.Contains(bodyLower, "deactivated")
}
