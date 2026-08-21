// Package sink adapts an agent turn's events onto a *tui.UI, keeping the front
// end in main thin. It imports pkg/agent only for the event types and maps each
// one to the corresponding TUI call; pkg/tui itself never sees the agent.
package sink

import (
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// Sink renders one agent turn onto the TUI. Rendering state lives on the UI
// itself.
//
// Concurrency: ToolStart, ToolOutput, Diff and the done hook may be called from
// parallel tool goroutines; *tui.UI serializes them. busy is only touched from
// the loop goroutine (TurnStart/TurnEnd) and must stay that way.
type Sink struct {
	ui   *tui.UI
	busy func() // clears the working spinner; nil while idle
}

// New returns a sink that drives ui.
func New(ui *tui.UI) *Sink { return &Sink{ui: ui} }

// TurnStart lights the working spinner until TurnEnd. The prompt is echoed at
// submission time, not here, so it lands above the line without waiting on the turn.
func (s *Sink) TurnStart(agent.TurnInfo) {
	s.busy = s.ui.Busy()
}

// UserPrompt echoes a prompt's words as committed history. Live sessions call
// ui.UserEcho at submission time; replay uses this so restored context shows each
// user message above its reply.
func (s *Sink) UserPrompt(text string) { s.ui.UserEcho(text) }

// Thinking streams reasoning output.
func (s *Sink) Thinking(delta string) { s.ui.Thinking(delta) }

// EndThinking flushes the thinking block before text starts.
func (s *Sink) EndThinking() { s.ui.EndThinking() }

// Text streams assistant markdown.
func (s *Sink) Text(delta string) { s.ui.Text(delta) }

// EndText renders whatever remains of the message.
func (s *Sink) EndText() { s.ui.EndText() }

// ToolStart maps a tool call onto the TUI spinner and returns the completion
// hook that reports how it ended. Incremental output is streamed separately via
// ToolOutput, so only an error or a Display string needs extra rendering here.
func (s *Sink) ToolStart(call agent.ToolCall, label string) func(agent.ToolResult) {
	if strings.TrimSpace(label) == "" {
		label = call.Name
	}
	done := s.ui.ToolStart(label)
	return func(res agent.ToolResult) {
		var result string
		if res.IsError {
			s.ui.Notify("tool "+call.Name+" failed", tui.LevelWarn)
			result = firstText(res.Content)
		} else if strings.TrimSpace(res.Display) != "" {
			// commit what history shows when it differs from the streamed output
			result = res.Display
		}
		done(result)
	}
}

// ToolOutput streams raw tool output a chunk at a time.
func (s *Sink) ToolOutput(callID, delta string) { s.ui.Output(delta) }

// ToolProgress shows a call the model is still composing as an activity row, so
// a long set of arguments reports its size instead of sitting silent. The row
// clears when the call is complete and its own header takes over.
func (s *Sink) ToolProgress(p agent.ToolProgress) {
	if p.Done {
		s.ui.SetActivity(progressKey(p.CallID), "")
		return
	}
	s.ui.SetActivity(progressKey(p.CallID), progressRow(p))
}

// progressKey namespaces a call's activity row against sub-agent rows.
func progressKey(callID string) string { return "call:" + callID }

// progressRow renders "write notes.go · 132 lines · 4.1k".
func progressRow(p agent.ToolProgress) string {
	parts := []string{p.Name}
	if p.Path != "" {
		parts = append(parts, p.Path)
	}
	row := strings.Join(parts, " ")
	if p.Lines > 0 {
		row += " · " + strconv.Itoa(p.Lines) + " lines"
	}
	return row + " · " + strutil.HumanSize(int64(p.Bytes))
}

// Diff commits a colorized file edit.
func (s *Sink) Diff(path, before, after string) { s.ui.Diff(path, before, after) }

// Usage is kept on the interface for pass-through sinks but no longer drives the
// bar; Context does. It renders nothing here.
func (s *Sink) Usage(llm.Usage) {}

// Context updates the context bar from the ledger's snapshot.
func (s *Sink) Context(c tokens.ContextState) {
	s.ui.SetContext(tui.ContextInfo{
		Used:      c.Used,
		Window:    c.Window,
		Reserve:   c.Reserve,
		Compact:   c.Compact,
		Estimated: c.Estimated,
	})
}

// Notice surfaces a message at the matching severity.
func (s *Sink) Notice(msg string, level agent.Level) {
	s.ui.Notify(msg, tui.Level(level))
}

// TurnEnd flushes any open tool output line and clears the working spinner.
// Failures and aborts already landed as notices from the loop; the context bar
// was updated by Context events during the turn.
func (s *Sink) TurnEnd(res agent.TurnResult) {
	s.ui.EndOutput()
	if s.busy != nil {
		s.busy()
		s.busy = nil
	}
}

// firstText returns the first non-empty text block, for a tool's error message.
func firstText(blocks llm.BlockList) string {
	for _, b := range blocks {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			return tb.Text
		}
	}
	return ""
}
