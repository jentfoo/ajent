package subagent

import (
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
)

// deltaFlush coalesces thinking/text deltas onto a single republish so a
// streaming response does not repaint the row per token.
const deltaFlush = 150 * time.Millisecond

// childSink publishes one activity row per running job: the current tool call or
// a "thinking…" line, elided to a single line and cleared on turn end. Nothing it
// emits ever reaches committed history — it feeds Options.Activity only.
type childSink struct {
	agent.NopSink
	id  string // row key, e.g. sub-2
	pub func(key, text string)

	mu      sync.Mutex
	timer   *time.Timer // pending coalesced flush; nil when none armed
	lastPub time.Time   // last publish wall clock, for delta throttling
	text    string      // newest desired row (full line incl. id prefix); "" clears
}

func newChildSink(id string, pub func(key, text string)) *childSink {
	return &childSink{id: id, pub: pub}
}

// set records the newest desired row. force publishes immediately; otherwise it is
// coalesced onto a short tick so thinking/text deltas do not repaint per token.
func (s *childSink) set(text string, force bool) {
	s.mu.Lock()
	if text == s.text && !force {
		s.mu.Unlock()
		return
	}
	s.text = text
	now := time.Now()
	switch {
	case force || now.Sub(s.lastPub) >= deltaFlush:
		s.flushLocked(now)
	case s.timer != nil: // a flush is already armed; it will pick up the latest text
	default:
		delay := deltaFlush - now.Sub(s.lastPub)
		s.timer = time.AfterFunc(delay, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.flushLocked(time.Now())
		})
	}
	s.mu.Unlock()
}

// flushLocked publishes the newest row and arms nothing further. Caller holds mu.
func (s *childSink) flushLocked(now time.Time) {
	if s.pub != nil {
		s.pub(s.id, s.text)
	}
	s.lastPub = now
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// ToolStart shows the tool's label; on completion it restores the prior line so a
// finished call falls back to thinking/idle text.
func (s *childSink) ToolStart(call agent.ToolCall, label string) func(agent.ToolResult) {
	s.mu.Lock()
	prev := s.text
	if prev == "" { // no coalesced line yet; show the idle fallback after this call
		prev = thinkingRow(s.id)
	}
	s.mu.Unlock()
	s.set(rowLine(s.id, oneLine(label)), true)
	return func(agent.ToolResult) {
		s.set(prev, true)
	}
}

// Thinking coalesces reasoning deltas onto a single "thinking…" line.
func (s *childSink) Thinking(string) { s.set(thinkingRow(s.id), false) }

// Text shows the child's most recent output line, so the row reads as live work
// rather than a static placeholder. Reasoning stays on thinkingRow via Thinking.
func (s *childSink) Text(text string) { s.set(rowLine(s.id, text), false) }

// TurnEnd clears the job's row; the activity cap and elision happen in tui.SetActivity.
func (s *childSink) TurnEnd(result agent.TurnResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = ""
	s.flushLocked(time.Now()) // drops any pending flush and publishes the clear
}

// oneLine collapses newlines and runs of whitespace to single spaces.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// thinkingRow is the coalesced idle line for a job.
func thinkingRow(id string) string { return id + "  thinking…" }

// rowLine prefixes an activity label with the job id, matching the "<id>  <label>" shape.
func rowLine(id, text string) string { return id + "  " + oneLine(text) }
