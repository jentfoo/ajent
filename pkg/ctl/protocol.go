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
const Protocol = 1

// Method names a controller may call. The read-only set answers any
// connection; every write method requires the control seat.
const (
	MethodState  = "state.get"
	MethodReplay = "events.replay"

	MethodAttach    = "attach"
	MethodSubmit    = "submit"
	MethodInterrupt = "interrupt"
	MethodModeCycle = "mode.cycle"
	MethodInputSet  = "input.set"
	MethodRewind    = "session.rewind"
	MethodResolve   = "interaction.resolve"
)

// Event names, grouped by family. Stream events mirror the agent sink one to
// one, session events the rendered surfaces outside the turn loop.
const (
	// EventHello greets a fresh connection: protocol version and session identity.
	EventHello = "hello"

	// EventTurnStart opens a turn; the stream events mirror the agent sink one
	// to one.
	EventTurnStart     = "turn.start"
	EventUserPrompt    = "user_prompt"
	EventThinkingDelta = "thinking.delta"
	EventThinkingEnd   = "thinking.end"
	EventTextDelta     = "text.delta"
	EventTextEnd       = "text.end"
	EventToolStart     = "tool.start"
	EventToolOutput    = "tool.output"
	EventToolProgress  = "tool.progress"
	EventToolResult    = "tool.result"
	EventDiff          = "diff"
	EventUsage         = "usage"
	EventContext       = "context"
	EventNotice        = "notice"
	EventTurnEnd       = "turn.end"

	// EventEcho re-emits one committed user message; the session events cover
	// the rendered surfaces outside the turn loop.
	EventEcho               = "echo"
	EventEchoImages         = "echo.images"
	EventPrint              = "print"
	EventEntryAppend        = "entry.append"
	EventStatus             = "status"
	EventActivity           = "activity"
	EventQueued             = "queued"
	EventEditor             = "editor"
	EventHead               = "head"
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
	CodeTooOld        = "too-old"
	CodeUnavailable   = "unavailable"
	CodeSeatHeld      = "seat-held" // another controller owns the seat
	CodeObserver      = "observer"  // mutating call from a connection without the seat
	CodeNotFound      = "not-found"
	CodeSettled       = "settled"
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

// Error is one legible refusal on a Reply.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
