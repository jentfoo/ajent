package llm

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/go-analyze/bulk"
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
	SessionID   string // session-affinity headers, when a provider supports them
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

// ReasoningConfig is what the user chose, before provider translation. It also
// round-trips through JSON as a session setting_change value.
type ReasoningConfig struct {
	Level  Level        `json:"level"`
	Budget int          `json:"budget,omitempty"` // explicit token budget, overrides Level when positive
	Retain RetainPolicy `json:"retain,omitempty"`
	Show   bool         `json:"show,omitempty"` // stream thinking to the UI
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

// allLevels is every reasoning depth in ascending order, the base for filtering.
func allLevels() []Level {
	return []Level{LevelOff, LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax}
}

// levelValue maps a non-off level onto the provider's effort value. An absent map
// key sends the level name itself; an explicit null reports unsupported.
func levelValue(caps Capabilities, l Level) (string, bool) {
	if v, ok := caps.LevelMap[l]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	return l.String(), true
}

// offValue maps the off level onto a provider value. An absent key uses def;
// an explicit null reports that thinking cannot be turned off.
func offValue(caps Capabilities, def string) (string, bool) {
	if v, ok := caps.LevelMap[LevelOff]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	return def, def != ""
}

// offSuppressed reports a model that cannot stop thinking (off maps to null).
func offSuppressed(caps Capabilities) bool {
	v, ok := caps.LevelMap[LevelOff]
	return ok && v == nil
}

// levelsFor returns the reasoning depths a model offers: only LevelOff when it
// cannot reason; otherwise every level without a null entry, where xhigh and max
// are opt-in via an explicit non-null map entry.
func levelsFor(caps Capabilities) []Level {
	if !caps.Reasoning {
		return []Level{LevelOff}
	}
	return bulk.SliceFilter(func(l Level) bool {
		v, ok := caps.LevelMap[l]
		switch {
		case ok && v == nil:
			return false // an explicit null removes the level
		case (l == LevelXHigh || l == LevelMax) && !ok:
			return false // xhigh and max are opt-in
		default:
			return true
		}
	}, allLevels())
}

// clampLevel snaps a requested reasoning depth to the nearest supported one,
// searching up then down and falling back to the first available. Off is never
// escalated: it always means no reasoning, even on a model that cannot stop.
func clampLevel(caps Capabilities, l Level) Level {
	if l == LevelOff {
		return LevelOff // requesting off stays off; {off:null} emits nothing
	}
	supported := levelsFor(caps)
	idx, found := slices.BinarySearch(supported, l)
	if found {
		return supported[idx]
	}
	if idx < len(supported) { // next higher level
		return supported[idx]
	}
	// highest below the request; off is always in levelsFor when reasoning,
	// so this only fires for a non-reasoning model clamped by mistake
	for i := len(supported) - 1; i >= 0; i-- {
		if supported[i] != LevelOff {
			return supported[i]
		}
	}
	return LevelOff
}

// LevelsFor returns the reasoning depths m offers, for menus and completion.
func LevelsFor(m Model) []Level { return levelsFor(m.Caps) }

// ClampLevel snaps a requested level onto one m actually supports.
func ClampLevel(m Model, l Level) Level { return clampLevel(m.Caps, l) }

// MaxOutputFor clamps an output cap to the tokens left in the window after input
// and reserve. It returns the model's own cap when the window is unknown, and the
// available window when no cap is set; never below one token.
func MaxOutputFor(m Model, inputTokens int) int {
	if m.ContextWindow <= 0 {
		return m.MaxOutput // window unknown: the model cap stands
	}
	out := m.MaxOutput
	if out <= 0 {
		out = max(1, m.ContextWindow-inputTokens)
	}
	available := m.ContextWindow - inputTokens - m.Reserve()
	return max(1, min(out, available))
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
