package llm

// EventType identifies which fields of an Event are set.
type EventType uint8

const (
	EventMessageStart EventType = iota
	EventThinkingStart
	EventThinkingDelta
	EventThinkingEnd
	EventTextStart
	EventTextDelta
	EventTextEnd
	EventToolCallStart
	EventToolCallDelta
	EventToolCallEnd
	EventUsage
	EventDone
)

// String returns the event name used in logs and golden fixtures.
func (t EventType) String() string {
	switch t {
	case EventMessageStart:
		return "message_start"
	case EventThinkingStart:
		return "thinking_start"
	case EventThinkingDelta:
		return "thinking_delta"
	case EventThinkingEnd:
		return "thinking_end"
	case EventTextStart:
		return "text_start"
	case EventTextDelta:
		return "text_delta"
	case EventTextEnd:
		return "text_end"
	case EventToolCallStart:
		return "tool_call_start"
	case EventToolCallDelta:
		return "tool_call_delta"
	case EventToolCallEnd:
		return "tool_call_end"
	case EventUsage:
		return "usage"
	case EventDone:
		return "done"
	default:
		return "unknown"
	}
}

// Event is one normalized step of a provider response stream. Which fields are
// set depends on Type: Text on the delta events, Block on the end events, Usage
// on EventUsage, StopReason and Err on EventDone, Meta on EventMessageStart.
type Event struct {
	Type       EventType
	Index      int    // content block index, pairs a start with its deltas and end
	Text       string // text or thinking delta, partial JSON on EventToolCallDelta
	ToolCallID string
	ToolName   string
	Block      Block // the complete block on the end events
	Usage      Usage
	StopReason StopReason
	Meta       *StreamMeta // EventMessageStart only
	Err        error       // EventDone when the turn failed
}

// StreamMeta is what the provider reports before any content.
type StreamMeta struct {
	Model     string
	RequestID string
}

// StopReason is why a response ended.
type StopReason uint8

const (
	StopUnknown StopReason = iota
	StopEndTurn
	StopToolUse
	StopMaxTokens
	StopAborted
	StopError
)

// String returns the stop reason name used in logs and golden fixtures.
func (s StopReason) String() string {
	switch s {
	case StopEndTurn:
		return "end_turn"
	case StopToolUse:
		return "tool_use"
	case StopMaxTokens:
		return "max_tokens"
	case StopAborted:
		return "aborted"
	case StopError:
		return "error"
	default:
		return "unknown"
	}
}

// Usage is the provider reported token accounting for one response. Phase 07
// owns aggregation; this is the per response report.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Reasoning  int // when reported separately
}

// Add folds o into u, for providers that report usage incrementally.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
}
