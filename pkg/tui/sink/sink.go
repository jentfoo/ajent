// Package sink adapts an agent turn's events onto a *tui.UI, keeping the front
// end in main thin. It imports pkg/agent only for the event types and maps each
// one to the corresponding TUI call; pkg/tui itself never sees the agent.
package sink

import (
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// Sink renders one agent turn onto the TUI. Rendering state lives on the UI
// itself.
type Sink struct {
	ui   *tui.UI
	toks int // last reported context size, so repeated Usage calls never shrink it

	busy func() // clears the working spinner; nil while idle
	mu   sync.Mutex
}

// New returns a sink that drives ui.
func New(ui *tui.UI) *Sink { return &Sink{ui: ui} }

// TurnStart echoes the prompt so a replay or resume shows each user message,
// and live turns render it too — startTurn no longer pre-echoes. It also lights
// the working spinner: from here until TurnEnd the model owns control, so there
// is always an indicator that something is in flight even before the first token.
func (s *Sink) TurnStart(info agent.TurnInfo) {
	if strings.TrimSpace(info.Input.Text) != "" {
		s.ui.UserEcho(info.Input.Text)
	}
	s.busy = s.ui.Busy()
}

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
// ToolOutput, so only an error needs extra rendering here.
func (s *Sink) ToolStart(call agent.ToolCall) func(agent.ToolResult) {
	done := s.ui.ToolStart(call.Name)
	return func(res agent.ToolResult) {
		if !res.IsError {
			// output already streamed through ToolOutput; nothing extra to commit
			done("")
			return
		}
		s.ui.Notify("tool "+call.Name+" failed", tui.LevelWarn)
		done(firstText(res.Content))
	}
}

// ToolOutput streams raw tool output a chunk at a time.
func (s *Sink) ToolOutput(callID, delta string) { s.ui.Output(delta) }

// Diff commits a colorized file edit.
func (s *Sink) Diff(path, before, after string) { s.ui.Diff(path, before, after) }

// Usage folds the latest provider accounting into the context bar.
func (s *Sink) Usage(u llm.Usage) {
	s.mu.Lock()
	// input grows with history; cacheRead counts reused prefix tokens
	if total := u.Input + u.CacheRead; total > s.toks {
		s.toks = total
	}
	s.ui.SetTokens(s.toks)
	s.mu.Unlock()
}

// Notice surfaces a message at the matching severity.
func (s *Sink) Notice(msg string, level agent.Level) {
	s.ui.Notify(msg, tui.Level(level))
}

// TurnEnd updates the context bar from the turn's totals, flushes any open
// tool output line and clears the working spinner. Failures and aborts already
// landed as notices from the loop.
func (s *Sink) TurnEnd(res agent.TurnResult) {
	s.ui.EndOutput()
	if s.busy != nil {
		s.busy()
		s.busy = nil
	}
	if n := res.Usage.Input + res.Usage.CacheRead; n > 0 {
		s.mu.Lock()
		if n > s.toks {
			s.toks = n
		}
		s.ui.SetTokens(s.toks)
		s.mu.Unlock()
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
