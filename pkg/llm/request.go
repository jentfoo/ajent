package llm

import (
	"context"
	"encoding/json"
)

// Request is one model call.
type Request struct {
	Model       Model
	System      BlockList
	Messages    []Message
	Tools       []ToolSchema
	ToolChoice  ToolChoice
	MaxTokens   int
	Temperature *float64 // nil uses the provider default
	Reasoning   ReasoningConfig
	Cache       CachePolicy
}

// ToolSchema is a tool as the model sees it.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

// ToolChoice constrains which tool the model may call.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // ToolChoiceSpecific only
}

// ToolChoiceMode is how tool selection is constrained.
type ToolChoiceMode uint8

const (
	ToolChoiceAuto ToolChoiceMode = iota
	ToolChoiceNone
	ToolChoiceRequired
	ToolChoiceSpecific
)

// CachePolicy asks the adapter to place prompt cache breakpoints.
type CachePolicy struct {
	Enabled bool
	// KeepLast is how many trailing message boundaries get a breakpoint beyond
	// the system prompt and tool definitions. Anthropic allows four breakpoints
	// in total, so the useful range is 0 to 2.
	KeepLast int
}

// Provider is one vendor endpoint. Capabilities are carried per model on
// Model.Caps rather than per provider, since quirks vary by model.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Counter is implemented by providers with a token counting endpoint. Callers
// type assert for it rather than switching on Capabilities.Tokenizer.
type Counter interface {
	CountTokens(ctx context.Context, req Request) (int, error)
}

// Discoverer is implemented by providers that can list their own models.
type Discoverer interface {
	DiscoverModels(ctx context.Context) ([]Model, error)
}

// ReasoningConfig is what the user chose, before provider translation.
type ReasoningConfig struct {
	Level  Level
	Budget int // explicit token budget, overrides Level when positive
	Retain RetainPolicy
	Show   bool // stream thinking to the UI
}

// Level is the requested reasoning depth. The standard seven levels let any
// thinkingLevelMap written against them map every entry.
type Level uint8

const (
	LevelOff Level = iota
	LevelMinimal
	LevelLow
	LevelMedium
	LevelHigh
	LevelXHigh
	LevelMax
)

var levelNames = enumNames[Level]{
	LevelOff:     "off",
	LevelMinimal: "minimal",
	LevelLow:     "low",
	LevelMedium:  "medium",
	LevelHigh:    "high",
	LevelXHigh:   "xhigh",
	LevelMax:     "max",
}

// String returns the configuration name of the level.
func (l Level) String() string { return levelNames.name(l) }

// MarshalText encodes the level as its configuration name.
func (l Level) MarshalText() ([]byte, error) { return levelNames.marshalText(l) }

// UnmarshalText decodes the configuration name.
func (l *Level) UnmarshalText(data []byte) error {
	return levelNames.unmarshalText(data, l, "reasoning level")
}

// ParseLevel returns the level named by s.
func ParseLevel(s string) (Level, bool) { return levelNames.lookup(s) }

// Levels returns every level in order, for menus and completion.
func Levels() []Level {
	return []Level{LevelOff, LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax}
}

// RetainPolicy is how much thinking survives into later requests. It applies at
// request build time only; the transcript always keeps everything.
type RetainPolicy uint8

const (
	// RetainNone strips thinking entirely. Providers that require replay are
	// upgraded to RetainWholeTurn, since the request would otherwise be invalid.
	RetainNone RetainPolicy = iota
	// RetainLastTurn keeps thinking on the most recent assistant message.
	RetainLastTurn
	// RetainWholeTurn keeps thinking for the current tool calling turn and
	// strips completed turns. This is the configured default.
	RetainWholeTurn
	// RetainAll never strips.
	RetainAll
)

var retainNames = enumNames[RetainPolicy]{
	RetainNone:      "none",
	RetainLastTurn:  "lastTurn",
	RetainWholeTurn: "wholeTurn",
	RetainAll:       "all",
}

// String returns the configuration name of the policy.
func (p RetainPolicy) String() string { return retainNames.name(p) }

// MarshalText encodes the policy as its configuration name.
func (p RetainPolicy) MarshalText() ([]byte, error) { return retainNames.marshalText(p) }

// UnmarshalText decodes the configuration name.
func (p *RetainPolicy) UnmarshalText(data []byte) error {
	return retainNames.unmarshalText(data, p, "retention policy")
}

// ParseRetain returns the retention policy named by s.
func ParseRetain(s string) (RetainPolicy, bool) { return retainNames.lookup(s) }
