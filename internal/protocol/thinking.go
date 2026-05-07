// Package protocol provides shared helpers for the ir sub-packages.
package protocol

// ThinkingLevelToBudget converts a discrete thinking level (used by OpenAI/Codex)
// to a numeric budget_tokens value (used by Claude/Gemini).
// Reference: CPA internal/thinking/convert.go
func ThinkingLevelToBudget(level string) int {
	switch level {
	case "none":
		return 0
	case "auto":
		return -1
	case "minimal":
		return 512
	case "low":
		return 1024
	case "medium":
		return 8192
	case "high":
		return 24576
	case "xhigh":
		return 32768
	case "max":
		return 128000
	default:
		return -1 // auto
	}
}

// ThinkingBudgetToLevel converts a numeric budget to a discrete level.
func ThinkingBudgetToLevel(budget int) string {
	switch {
	case budget <= 0:
		return "auto"
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	case budget <= 32768:
		return "xhigh"
	default:
		return "max"
	}
}

// MapToClaudeEffort converts a generic thinking level to Claude's adaptive
// effort values (low/medium/high/max).
func MapToClaudeEffort(level string) string {
	switch level {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "max"
	default:
		return "high"
	}
}

// MapToOpenAIEffort converts a generic thinking level to OpenAI's reasoning_effort.
func MapToOpenAIEffort(level string) string {
	switch level {
	case "none":
		return "low"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	default:
		return "medium"
	}
}
