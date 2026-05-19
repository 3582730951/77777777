package scheduler

import (
	"errors"
	"testing"

	"github.com/llm-pool/gateway/internal/domain"
)

func TestClassifyBannedSignals(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  error
	}{
		{name: "account deactivated body", body: `{"error":{"code":"account_deactivated"}}`},
		{name: "workspace deactivated spaced", body: `unexpected status 402 Payment Required: detail: code deactivated workspace`},
		{name: "workspace deactivated hyphen", body: `workspace-deactivated`},
		{name: "team deactivated", body: `unexpected upstream code: team_deactivated`},
		{name: "generic deactivated", body: `auth error: account deactivated`},
		{name: "workspace disabled body", body: `organization has been disabled`},
		{name: "terminated body", body: `your access was terminated`},
		{name: "error string", err: errors.New("account_disabled: account is disabled")},
	}
	for _, tc := range cases {
		if got := ClassifyError(403, tc.body, tc.err); got != domain.ErrBanned {
			t.Fatalf("%s classified as %s", tc.name, got)
		}
	}
}

func TestClassifyUsageLimitTryAgainSignal(t *testing.T) {
	body := `{"error":{"message":"Usage limit reached. Please try again later."}}`
	if got := ClassifyError(429, body, nil); got != domain.ErrQuotaExhausted {
		t.Fatalf("usage limit try-again signal classified as %s", got)
	}
}

func TestClassifyCapacitySignalOverridesLegacyQuotaPrefix(t *testing.T) {
	err := errors.New("upstream quota: Selected model is at capacity. Please try a different model.")
	if got := ClassifyError(0, "", err); got != domain.ErrRateLimited {
		t.Fatalf("capacity signal classified as %s", got)
	}
}
