package domain

import "time"

type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

type Group struct {
	ID              string
	TenantID        string
	Provider        string
	APIKeys         []string
	Models          []string
	AccountIDs      []string
	ModelAliases    map[string]string
	ModelWhitelist  []string
	APIKeyOverrides map[string]APIKeyOverride
	// SystemPrompt is injected into every request for this group.
	SystemPrompt     string
	SystemPromptMode string // "prepend" | "append" | "replace"
	// ReasoningEffort overrides the client's reasoning_effort for all requests in this group.
	// Codex: "low" | "medium" | "high" | "xhigh"
	// Claude: "none" | "low" | "medium" | "high" | "max" (mapped to budget_tokens)
	// Empty = use client's value or provider default.
	ReasoningEffort string
	// ForcedModel overrides the model for all requests in this group.
	// Client's requested model is ignored when this is set.
	// Useful for pinning a group to a specific model or aliasing across providers.
	ForcedModel    string
	SourcePlatform string
	AutoRegister   bool
}

type APIKeyOverride struct {
	ModelAliases map[string]string
}

type Federation struct {
	ID           string
	Description  string
	MemberGroups []string
	APIKeys      []string
	TenantID     string
}

type AccountState string

const (
	StateActive       AccountState = "active"
	StateCooling      AccountState = "cooling"
	StateBanned       AccountState = "banned"
	StateCFChallenged AccountState = "cf_challenged"
	StateDisabled     AccountState = "disabled"
)

type AvailConfidence int

const (
	ConfConfirmedAvailable AvailConfidence = iota
	ConfLikelyAvailable
	ConfSuspectedIssue
	ConfProbablyExhausted
	ConfCooling
)

func (c AvailConfidence) String() string {
	switch c {
	case ConfConfirmedAvailable:
		return "confirmed_available"
	case ConfLikelyAvailable:
		return "likely_available"
	case ConfSuspectedIssue:
		return "suspected_issue"
	case ConfProbablyExhausted:
		return "probably_exhausted"
	case ConfCooling:
		return "cooling"
	}
	return "unknown"
}

type BreakerState int

const (
	BreakerClosed BreakerState = iota
	BreakerOpen
	BreakerHalfOpen
)

type Account struct {
	ID             string
	TenantID       string
	Provider       string
	Email          string
	CredentialBlob []byte
	StealthProfile string
	UA             string
	Proxy          string
	State          AccountState
	PlanTier       string
	Quota          QuotaState
	Health         HealthState
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type QuotaWindow struct {
	Used       float64
	Limit      float64
	ResetAt    time.Time
	Confidence float32
}

type ExtraUsage struct {
	IsEnabled    bool
	MonthlyLimit float64
	UsedCredits  float64
	Utilization  float64
	Currency     string
}

type QuotaState struct {
	ShortWindow       QuotaWindow
	LongWindow        QuotaWindow
	TierWindows       map[string]QuotaWindow
	ExtraUsage        *ExtraUsage
	DiscoveredModels  []ModelCapability
	PlanTier          string
	LastDiscoveryAt   time.Time
	LastRateLimitedAt time.Time
}

type ModelCapability struct {
	ID             string
	Available      bool
	RateLimit      int
	SupportsTools  bool
	SupportsVision bool
	ContextWindow  int
}

type HealthState struct {
	EWMALatency   float64
	SuccessRate   float64
	InflightReqs  int
	Confidence    AvailConfidence
	Breaker       BreakerState
	OpenUntil     time.Time
	FailCount     int
	LastSuccessAt time.Time
	LastFailureAt time.Time
}

type ErrorClass string

const (
	ErrQuotaExhausted ErrorClass = "quota_exhausted"
	ErrRateLimited    ErrorClass = "rate_limited"
	ErrCFChallenge    ErrorClass = "cf_challenge"
	ErrAuthFailed     ErrorClass = "auth_failed"
	ErrBanned         ErrorClass = "banned"
	ErrNetwork        ErrorClass = "network_error"
	ErrUpstreamError  ErrorClass = "upstream_error"
	ErrUnknown        ErrorClass = "unknown"
)
