// Package agent runs the prompt -> stream -> tool-call -> repeat loop that
// turns a model provider into an interactive coding agent, and emits events on
// a Sink so any front end (the interactive TUI, a JSON line writer in headless
// mode, or nothing at all for a sub-agent) can render it.
package agent

import (
	"context"
	"encoding/json"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// Input is one user turn: free text, extra content blocks, and the synthetic
// messages that frame it. Before is context that predates the message (staged
// shell results, a project survey); After is context the message asked for, so
// it is resolved when the message lands and a rewind past it drops both.
type Input struct {
	Text      string
	Blocks    llm.BlockList                       // extra content, appended after Text when non-empty
	Before    []llm.Message                       // appended ahead of this input, in transcript order
	After     func(context.Context) []llm.Message // appended behind it once it lands; nil is the normal case
	Delivered func()                              // called once the message lands, ahead of After; nil is the normal case
	Settled   func()                              // called once After has landed too; nil is the normal case
	Injected  bool                                // system-injected context (not a typed prompt); excluded from recall
}

// State is the in-memory projection of a session. It is owned by the loop
// goroutine; only Agent.mu guards the queue and running flag, never this.
type State struct {
	Messages  []llm.Message
	Model     llm.Model
	Reasoning llm.ReasoningConfig
	Tools     []string // active tool names, in declaration order
	Tokens    *tokens.Accounting
}

// Transform rewrites an assembled message list before it is sent, never by
// mutating State.
type Transform func([]llm.Message) []llm.Message

// TurnInfo describes one turn to the sink when it starts.
type TurnInfo struct {
	Model llm.Model
	Input Input
}

// TurnResult reports how a turn ended, for the sink and the session log.
type TurnResult struct {
	Stop  llm.StopReason
	Usage llm.Usage
	Steps int
	Err   error // transport or context failure; tool errors are results not this
}

// MessageInfo is one appended message and what the stream reported with it.
type MessageInfo struct {
	Message  llm.Message
	Stop     llm.StopReason // assistant messages only
	Usage    llm.Usage
	Injected bool // system-injected context, excluded from prompt recall
}

// ToolCall is one model-requested invocation handed to a Tool.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is what the loop feeds back to the model for one call. An erroring
// tool still returns here with IsError set, so the turn continues rather than
// aborting.
type ToolResult struct {
	Content llm.BlockList // what the model sees
	// Display is what history shows. A tool either streams to agent.Output or sets
	// this, never both; otherwise its head renders twice.
	Display string
	Details any // structured detail for extensions and the transcript
	IsError bool
	// EndTurn stops the turn once this call's results are appended, with no
	// further model call. Control tools set it to hand a phase over.
	EndTurn bool
}
