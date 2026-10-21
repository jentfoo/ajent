package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// toolAccumulator assembles tool calls from chat-completions deltas, which
// arrive as unframed JSON fragments keyed by an array index. The wire index is
// stable only within one batch: a proxy may restart numbering inside a single
// response, so an arrival on a used index forks or fails rather than merges.
type toolAccumulator struct {
	blocks *blockCounter
	order  []*partialCall       // creation order, so a reused index keeps stream order
	calls  map[int]*partialCall // wire index -> the call currently receiving fragments
}

// partialCall is one tool call being assembled.
type partialCall struct {
	wire    int
	block   int
	id      string
	name    string
	args    strings.Builder
	started bool
	closed  bool
	buffer  []string // fragments seen before the call could be announced
	// prev is the closed call that last held the same wire index, set when this
	// call opened from fragments no id or name could attribute. A fresh id or
	// name clears it; otherwise Close fails the call instead of merging.
	prev *partialCall
}

// newToolAccumulator returns an accumulator claiming block indexes from blocks.
func newToolAccumulator(blocks *blockCounter) *toolAccumulator {
	return &toolAccumulator{blocks: blocks, calls: make(map[int]*partialCall)}
}

// Delta folds one tool_calls entry, returning the events it produced. A fragment
// arriving before the call's id and name are known is buffered, so a caller
// never sees a delta for a call it was not told about.
func (a *toolAccumulator) Delta(index int, id, name, fragment string) []Event {
	c, ok := a.calls[index]
	var events []Event
	if !ok {
		c = a.newCall(index)
	} else {
		switch {
		case freshIdentity(c, id, name):
			// a distinct invocation reusing the index: the previous call ends
			// here, so each keeps its own arguments and block
			events = append(events, a.closeCall(c))
			c = a.newCall(index)
		case c.closed:
			if id == "" && name == "" && fragment == "" {
				return nil // empty keep-alive entry: nothing to attribute, nothing to fail
			}
			// reuse after close carrying nothing to attribute by: the fragments
			// open an unnamed call that Close fails visibly, unless a fresh id
			// or name arrives later to claim them
			prev := c
			c = a.newCall(index)
			c.prev = prev
		case c.prev != nil && freshIdentity(c.prev, id, name):
			c.prev = nil // the arrival names the call the earlier fragments belong to
		}
	}
	// some proxies send an empty name on the first chunk and the real one after
	if id != "" {
		c.id = id
	}
	if name != "" {
		c.name = name
	}

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

// newCall registers a call receiving a wire index's fragments, claiming its
// block index and appending to the close order.
func (a *toolAccumulator) newCall(wire int) *partialCall {
	c := &partialCall{wire: wire, block: a.blocks.alloc()}
	a.calls[wire] = c
	a.order = append(a.order, c)
	return c
}

// freshIdentity reports whether an arrival on a used index carries an id or name
// that can only belong to a new invocation: both sides known and different. A
// value arriving where none was recorded is a late id or name, which proxies
// send mid-call, and never starts a new call.
func freshIdentity(c *partialCall, id, name string) bool {
	return (id != "" && c.id != "" && id != c.id) ||
		(name != "" && c.name != "" && name != c.name)
}

// Close finishes every open call in arrival order.
//
// Completion is decided by this boundary rather than by the arguments parsing,
// because partial JSON parses successfully far more often than not.
func (a *toolAccumulator) Close() []Event {
	events := make([]Event, 0, len(a.order))
	for _, c := range a.order {
		if c.closed {
			continue
		}
		events = append(events, a.closeCall(c))
	}
	return events
}

// closeCall ends one call, carrying its arguments and any failure on the end
// event, so a caller answers the call instead of losing it.
func (a *toolAccumulator) closeCall(c *partialCall) Event {
	c.closed = true
	if c.prev != nil {
		// never claimed: the fragments could extend the closed predecessor or
		// belong to a call the provider never named, so failing this call is the
		// honest outcome. The raw arguments ride along as evidence.
		raw := c.rawArgs()
		return Event{
			Type: EventToolCallEnd, Index: c.block, ToolCallID: c.id, ToolName: c.name,
			Block: ToolCallBlock{ID: c.id, Name: c.name, Input: json.RawMessage(raw)},
			Err: fmt.Errorf("%w: arguments arrived on index %d after its call closed, "+
				"with no id or name to attribute them by: %s",
				ErrReusedToolIndex, c.wire, strutil.Clip(raw, 120)),
		}
	}
	input, err := finishToolInput(c.args.String())
	return Event{
		Type: EventToolCallEnd, Index: c.block, ToolCallID: c.id, ToolName: c.name,
		Block: ToolCallBlock{ID: c.id, Name: c.name, Input: input},
		Err:   err,
	}
}

// rawArgs returns every fragment received, buffered or streamed.
func (c *partialCall) rawArgs() string {
	if len(c.buffer) == 0 {
		return c.args.String()
	}
	return strings.Join(c.buffer, "") + c.args.String()
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
