package llm

import (
	"encoding/json"
	"slices"
)

// Stream is a single model response, pulled one event at a time.
//
// Next blocks until the next event. It returns false at end of stream, after
// which Err reports any failure. Close is safe to call from another goroutine
// and unblocks a blocked Next; a deliberate Close leaves Err nil.
type Stream interface {
	Next() (Event, bool)
	Err() error
	Close() error
}

// Accumulator rebuilds the assistant message from a stream's events, so a
// caller can forward deltas to the UI in the same loop it assembles them.
type Accumulator struct {
	blocks  BlockList
	partial map[int]*partialBlock
	usage   Usage
	stop    StopReason
	meta    StreamMeta
	err     error
}

// partialBlock buffers deltas in case the stream ends before its end event.
type partialBlock struct {
	typ   BlockType
	text  string
	id    string
	name  string
	index int
	done  bool
}

// Add folds one event into the assembled message.
func (a *Accumulator) Add(ev Event) {
	switch ev.Type {
	case EventMessageStart:
		if ev.Meta != nil {
			a.meta = *ev.Meta
		}
	case EventThinkingStart:
		a.open(ev.Index, BlockThinking, "", "")
	case EventTextStart:
		a.open(ev.Index, BlockText, "", "")
	case EventToolCallStart:
		a.open(ev.Index, BlockToolCall, ev.ToolCallID, ev.ToolName)
	case EventThinkingDelta, EventTextDelta, EventToolCallDelta:
		if p := a.partial[ev.Index]; p != nil {
			p.text += ev.Text
		}
	case EventThinkingEnd, EventTextEnd, EventToolCallEnd:
		a.close(ev)
	case EventUsage:
		a.usage = ev.Usage // last wins
	case EventDone:
		a.stop = ev.StopReason
		if ev.Err != nil {
			a.err = ev.Err
		}
	}
}

// open records the start of a content block.
func (a *Accumulator) open(index int, typ BlockType, id, name string) {
	if a.partial == nil {
		a.partial = make(map[int]*partialBlock)
	}
	a.partial[index] = &partialBlock{typ: typ, id: id, name: name, index: index}
}

// close appends the completed block, preferring the one the event carries.
func (a *Accumulator) close(ev Event) {
	p := a.partial[ev.Index]
	if p != nil {
		p.done = true
	}
	if ev.Block != nil {
		a.blocks = append(a.blocks, ev.Block)
	} else if p != nil {
		a.blocks = append(a.blocks, p.build())
	}
}

// build reconstructs a block from buffered deltas, for a stream that ended
// before its end event arrived.
func (p *partialBlock) build() Block {
	switch p.typ {
	case BlockThinking:
		return ThinkingBlock{Text: p.text}
	case BlockToolCall:
		input := json.RawMessage(p.text)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		return ToolCallBlock{ID: p.id, Name: p.name, Input: input}
	default:
		return TextBlock{Text: p.text}
	}
}

// Message returns the assembled assistant message, including any block whose
// end event never arrived.
func (a *Accumulator) Message() Message {
	pending := a.pendingIndexes()
	if len(pending) == 0 {
		return Message{Role: RoleAssistant, Content: a.blocks}
	}
	blocks := slices.Clone(a.blocks)
	for _, i := range pending {
		blocks = append(blocks, a.partial[i].build())
	}
	return Message{Role: RoleAssistant, Content: blocks}
}

// pendingIndexes returns the indexes of blocks opened but never closed, in
// index order.
func (a *Accumulator) pendingIndexes() []int {
	var out []int
	for i, p := range a.partial {
		if !p.done {
			out = append(out, i)
		}
	}
	slices.Sort(out)
	return out
}

// Usage returns the last usage the provider reported.
func (a *Accumulator) Usage() Usage { return a.usage }

// StopReason returns why the response ended.
func (a *Accumulator) StopReason() StopReason { return a.stop }

// Meta returns what the provider reported before any content.
func (a *Accumulator) Meta() StreamMeta { return a.meta }

// Err returns the error carried on EventDone, if any.
func (a *Accumulator) Err() error { return a.err }

// Accumulate drains s and returns the assembled assistant message. Use an
// Accumulator directly when the caller also needs each event.
func Accumulate(s Stream) (Message, Usage, error) {
	var a Accumulator
	for ev, ok := s.Next(); ok; ev, ok = s.Next() {
		a.Add(ev)
	}
	err := s.Err()
	if err == nil {
		err = a.Err()
	}
	return a.Message(), a.Usage(), err
}

// SliceStream is a Stream over a fixed event slice, for tests and fakes.
type SliceStream struct {
	Events []Event
	Error  error

	pos    int
	closed bool
}

// Next returns the next event, or false once the slice is drained or closed.
func (s *SliceStream) Next() (Event, bool) {
	if s.closed || s.pos >= len(s.Events) {
		return Event{}, false
	}
	ev := s.Events[s.pos]
	s.pos++
	return ev, true
}

// Err returns the configured error.
func (s *SliceStream) Err() error { return s.Error }

// Close stops the stream.
func (s *SliceStream) Close() error {
	s.closed = true
	return nil
}
