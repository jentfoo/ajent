package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// toolAccumulator assembles tool calls from chat-completions deltas, which
// arrive as unframed JSON fragments keyed by an array index.
type toolAccumulator struct {
	base  int // block index of the first tool call
	order []int
	calls map[int]*partialCall
}

// partialCall is one tool call being assembled.
type partialCall struct {
	block   int
	id      string
	name    string
	args    strings.Builder
	started bool
	closed  bool
	buffer  []string // fragments seen before the call could be announced
}

// newToolAccumulator returns an accumulator assigning block indexes from base.
func newToolAccumulator(base int) *toolAccumulator {
	return &toolAccumulator{base: base, calls: make(map[int]*partialCall)}
}

// Delta folds one tool_calls entry, returning the events it produced. A fragment
// arriving before the call's id and name are known is buffered, so a caller
// never sees a delta for a call it was not told about.
func (a *toolAccumulator) Delta(index int, id, name, fragment string) []Event {
	c, ok := a.calls[index]
	if !ok {
		c = &partialCall{block: a.base + len(a.order)}
		a.calls[index] = c
		a.order = append(a.order, index)
	}
	// some proxies send an empty name on the first chunk and the real one after
	if id != "" {
		c.id = id
	}
	if name != "" {
		c.name = name
	}

	var events []Event
	if !c.started && c.name != "" {
		c.started = true
		events = append(events, Event{
			Type: EventToolCallStart, Index: c.block, ToolCallID: c.id, ToolName: c.name,
		})
		for _, b := range c.buffer {
			c.args.WriteString(b)
			events = append(events, Event{
				Type: EventToolCallDelta, Index: c.block, ToolCallID: c.id, Text: b,
			})
		}
		c.buffer = nil
	}
	if fragment == "" {
		return events
	}
	if !c.started {
		c.buffer = append(c.buffer, fragment)
		return events
	}
	c.args.WriteString(fragment)
	return append(events, Event{
		Type: EventToolCallDelta, Index: c.block, ToolCallID: c.id, Text: fragment,
	})
}

// Close finishes every open call in arrival order.
//
// Completion is decided by this boundary rather than by the arguments parsing,
// because partial JSON parses successfully far more often than not.
func (a *toolAccumulator) Close() []Event {
	events := make([]Event, 0, len(a.order))
	for _, idx := range a.order {
		c := a.calls[idx]
		if c.closed {
			continue
		}
		c.closed = true

		input, err := finishToolInput(c.args.String())
		ev := Event{
			Type: EventToolCallEnd, Index: c.block, ToolCallID: c.id, ToolName: c.name,
			Block: ToolCallBlock{ID: c.id, Name: c.name, Input: input},
			Err:   err,
		}
		events = append(events, ev)
	}
	return events
}

// finishToolInput validates accumulated arguments. Malformed JSON fails the one
// call rather than the turn, so the model can be told and try again.
func finishToolInput(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == `""` {
		return json.RawMessage("{}"), nil // several local models mean "no arguments"
	} else if !json.Valid([]byte(raw)) {
		return json.RawMessage(raw), fmt.Errorf("%w: %s", ErrMalformedToolArgs, strutil.Clip(raw, 120))
	}
	return json.RawMessage(raw), nil
}
