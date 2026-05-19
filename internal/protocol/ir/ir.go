// Package ir defines the intermediate representation that bridges all
// supported wire protocols (OpenAI Chat Completions, Anthropic Messages,
// Gemini generateContent). Decoders read protocol-specific request bodies
// into Request; encoders consume an event channel produced by a provider
// and emit a protocol-specific SSE stream.
package ir

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type PartKind int

const (
	PartText PartKind = iota
	PartImage
	PartToolUse
	PartToolResult
)

type Part struct {
	Kind PartKind

	Text string

	ImageURL   string
	ImageBytes []byte
	ImageMedia string

	ToolUseID    string
	ToolUseName  string
	ToolUseInput []byte

	ToolResultID    string
	ToolResultBytes []byte
	ToolResultErr   bool

	// CacheBreakpoint marks this part for Anthropic cache_control injection.
	CacheBreakpoint bool
	// CacheControl preserves an inbound Anthropic cache_control object.
	CacheControl []byte
}

type Message struct {
	Role  Role
	Parts []Part
}

type ToolDef struct {
	Name        string
	Description string
	Schema      []byte
	// CacheBreakpoint, when true, tells the anthropic encoder to inject
	// cache_control:{type:"ephemeral"} on this tool — marks the tools
	// block as a cacheable prefix for subsequent turns.
	CacheBreakpoint bool
	// CacheControl preserves an inbound Anthropic cache_control object.
	CacheControl []byte
}

type ToolChoice struct {
	Mode string
	Name string
}

type Request struct {
	Model  string
	System string
	// SystemCached, when true, tells the anthropic encoder to wrap the
	// system prompt in a cache_control:{type:"ephemeral"} block.
	// Set by cacheopt.InjectCacheBreakpoints when the system is large enough.
	SystemCached bool
	Messages     []Message
	Tools        []ToolDef
	ToolChoice   ToolChoice
	Temperature  *float64
	TopP         *float64
	MaxTokens    int
	Stream       bool
	ServiceTier  string

	ReasoningEffort string
	ThinkingTokens  int
	ThinkingType    string
	MessageCacheIdx int // ≥0: inject cache_control on Messages[idx]'s last content block

	OriginalModel string
	OriginalProto string

	// UpstreamSessionKey is a gateway-local stable key used by providers that
	// need a per-conversation upstream session id when the inbound protocol did
	// not already provide one. It is never sent upstream directly.
	UpstreamSessionKey string

	AnthropicSystem            []byte
	AnthropicSystemText        string
	AnthropicMetadata          []byte
	AnthropicContextManagement []byte
	AnthropicToolChoice        []byte
}

type EventKind int

const (
	EvTextDelta EventKind = iota
	EvToolUseStart
	EvToolUseDelta
	EvToolUseEnd
	EvThinkingDelta
	EvUsage
	EvDone
	EvError
)

type Event struct {
	Kind EventKind

	Text string

	ToolID    string
	ToolName  string
	ToolDelta []byte

	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int

	FinishReason string

	Err error
}
