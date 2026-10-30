// Package ctl holds the wire types and line codec of the ajent control
// socket: newline-delimited JSON over a unix socket, one request per line from
// the controller, replies and events back. Both sides (the server in pkg/app
// and any client) import this package, so it depends on nothing internal and
// every shape here is the versioned wire contract.
package ctl

import (
	"encoding/json"
	"fmt"
)

// Protocol is the wire protocol version announced in the hello event. Bump on
// any incompatible change to an event, reply or method shape.
//
// v2 splits events into a committed family reconciled by entry id (entry.append,
// branch.changed) and an ephemeral family governed by the watermark alone, and
// deletes the echo concepts that made dedupe the client's job.
//
// v3 gives queue rows stable ids: queued and queue.dropped carry {id, text}
// rows (secret texts redact to empty, never dropped), state.get reports rows,
// and submit takes an optional pendingId echoed back as the row id. Pending
// prompts reconcile by id instead of matching strings against strings.
const Protocol = 3

// Method names a controller may call. The read-only set answers any
// connection; every write method requires the control seat.
const (
	MethodState = "state.get"

	MethodAttach      = "attach"
	MethodSubmit      = "submit"
	MethodInterrupt   = "interrupt"
	MethodModeCycle   = "mode.cycle"
	MethodInputSet    = "input.set"
	MethodQueueRecall = "queue.recall"
	MethodRewind      = "session.rewind"
	MethodResolve     = "interaction.resolve"
	MethodSeatYield   = "seat.yield"
)

// Event names, grouped by family. Stream events mirror the agent sink one to
// one, session events the rendered surfaces outside the turn loop.
const (
	// EventHello greets a fresh connection: protocol version and session identity.
	EventHello = "hello"

	// EventTurnStart opens a turn; the stream events mirror the agent sink one
	// to one and belong to the ephemeral family.
	EventTurnStart     = "turn.start"
	EventThinkingDelta = "thinking.delta"
	EventThinkingEnd   = "thinking.end"
	EventTextDelta     = "text.delta"
	EventTextEnd       = "text.end"
	EventToolStart     = "tool.start"
	EventToolOutput    = "tool.output"
	EventToolProgress  = "tool.progress"
	EventToolResult    = "tool.result"
	EventDiff          = "diff"
	EventContext       = "context"
	EventNotice        = "notice" // transient notices only; recorded ones ride entry.append
	EventTurnEnd       = "turn.end"

	// EventPrint covers the rendered surfaces outside the turn loop. Committed
	// content is entry.append and branch.changed, reconciled by entry id;
	// everything else is ephemeral, governed by the watermark alone.
	EventPrint              = "print"
	EventEntryAppend        = "entry.append"
	EventBranchChanged      = "branch.changed"
	EventStatus             = "status"
	EventActivity           = "activity"
	EventQueued             = "queued"
	EventQueueDropped       = "queue.dropped"
	EventPermission         = "permission"
	EventTools              = "tools"
	EventIdle               = "idle"
	EventSessionRenamed     = "session.renamed"
	EventInteractionPending = "interaction.pending"
	EventInteractionSettled = "interaction.settled"

	// EventSeatLost announces a force takeover of the control seat: the displaced
	// holder downgrades to observer without probing with a write, and observers
	// learn who drives now. Never fires on release-by-disconnect.
	EventSeatLost = "seat.lost"

	// EventRefused replaces hello when the connection is not admitted: the
	// kernel-verified peer fails the session's identity check. Peers read it,
	// then the socket closes.
	EventRefused = "refused"

	// EventSessionExit is the last event before the socket closes.
	EventSessionExit = "session.exit"
)

// Error codes replies carry beside their message.
const (
	CodeUnknownMethod = "unknown-method"
	CodeBadRequest    = "bad-request"
	CodeUnavailable   = "unavailable"
	CodeSeatHeld      = "seat-held" // another controller owns the seat; Error.Holder names it
	CodeObserver      = "observer"  // mutating call from a connection without the seat
	CodeNotFound      = "not-found"
	CodeSettled       = "settled"
	CodeBusy          = "busy" // the target queue is full; retry once it drains
)

// Request is one controller line: a method call carrying a caller-chosen id
// the reply echoes.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Reply answers one Request by id. Seq is the watermark the result was taken
// at: events at or below it are superseded by the result, events above it
// apply on top.
type Reply struct {
	ID     string          `json:"id"`
	Seq    uint64          `json:"seq"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is one legible refusal on a Reply. Holder is set on seat-held
// refusals, so a script can decide between takeover, waiting and exiting
// without parsing the message.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Holder  string `json:"holder,omitempty"`
}

// Errf builds a Reply error from a code and formatted message.
func Errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Event is one server line a controller receives besides replies: everything
// the local front end would render, in arrival order. Seq is dense and
// monotonic for the session lifetime.
type Event struct {
	Name string          `json:"event"`
	Seq  uint64          `json:"seq"`
	Data json.RawMessage `json:"data,omitempty"`
}

// NewEvent builds one event, encoding data once. An unencodable payload drops
// rather than corrupting the stream.
func NewEvent(name string, seq uint64, data any) Event {
	return Event{Name: name, Seq: seq, Data: EncodeData(data)}
}

// EncodeData marshals one event payload; an unencodable value drops. Callers
// that assign the seq under a lock encode before taking it.
func EncodeData(data any) json.RawMessage {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil // dropped rather than corrupting the stream
	}
	return raw
}

// Decode unmarshals the event payload into v. An event without a payload
// leaves v untouched.
func (e Event) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// DecodeResult unmarshals a reply's result into v. An error reply reports its
// message instead.
func (r Reply) DecodeResult(v any) error {
	if r.Error != nil {
		return r.Error
	}
	if len(r.Result) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result, v)
}

// Error satisfies error so a reply's failure can travel as one.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Incoming is one decoded server line: exactly one of Reply or Event.
type Incoming struct {
	Reply *Reply
	Event *Event
}

// DecodeIncoming parses one server line into its Reply or Event form.
func DecodeIncoming(line []byte) (Incoming, error) {
	var probe struct {
		ID    string `json:"id"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return Incoming{}, err
	}
	if probe.Event != "" {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return Incoming{}, err
		}
		return Incoming{Event: &ev}, nil
	}
	var rp Reply
	if err := json.Unmarshal(line, &rp); err != nil {
		return Incoming{}, err
	}
	return Incoming{Reply: &rp}, nil
}
