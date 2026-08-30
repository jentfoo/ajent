package subagent

import (
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// deltaFlush coalesces thinking/text deltas onto a single republish so a
// streaming response does not repaint the row per token.
const deltaFlush = 150 * time.Millisecond

// maxBuf caps how much raw streamed text one row accumulates; on overflow only
// the head is kept (the display always shows a clean first portion of the line,
// never jumping to its tail), and tui.SetActivity elides it to window width.
const maxBuf = 2048

// Which stream owns buf: thinking reasoning or assistant text. They alternate
// within a turn, so switching streams starts the row fresh on the new content.
type stream uint8

const (
	streamNone     stream = iota // no deltas yet this turn
	streamThinking               // reasoning block in progress
	streamText                   // assistant prose in progress
)

// childSink publishes one activity row per running job: the current tool call or
// a "thinking..." line, elided to a single line. The row lives as long as the job
// does — Manager.spawn clears it on every terminal path — so a child that runs a
// second turn never blinks out of the list. Nothing it emits ever reaches
// committed history; it feeds Options.Activity only.
type childSink struct {
	agent.NopSink
	id   string // row key, e.g. sub-2
	rank int    // job number; the row's stable place in the activity list
	pub  func(key, text string, rank int)

	mu      sync.Mutex
	timer   *time.Timer // pending coalesced flush; nil when none armed
	lastPub time.Time   // last publish wall clock, for delta throttling
	text    string      // newest desired row (full line incl. id prefix); "" clears
	buf     string      // accumulated deltas of the current in-progress line
	src     stream      // which stream owns buf; switching resets it
	calls   int         // child tool calls in flight; the row falls back at zero
	idle    string      // row to restore once the last in-flight call ends
}

func newChildSink(id string, rank int, pub func(key, text string, rank int)) *childSink {
	return &childSink{id: id, rank: rank, pub: pub}
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
		s.pub(s.id, s.text, s.rank)
	}
	s.lastPub = now
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// ToolStart shows the running call plus its first argument (built-in labels like
// "read" are bare words). Child tools run in parallel, so the fallback is counted
// rather than captured per call: only the last call to finish restores the idle
// line, otherwise an early finisher would erase a sibling still running.
func (s *childSink) ToolStart(call agent.ToolCall, label string) func(agent.ToolResult) {
	s.mu.Lock()
	if s.calls == 0 { // first of a batch; remember what the row showed before it
		s.idle = s.text
		if s.idle == "" { // no coalesced line yet; fall back to the idle text
			s.idle = thinkingRow(s.id)
		}
	}
	s.calls++
	s.mu.Unlock()
	s.set(rowLine(s.id, toolLabel(call, label)), true)
	return func(agent.ToolResult) {
		s.mu.Lock()
		if s.calls > 0 {
			s.calls--
		}
		last, idle := s.calls == 0, s.idle
		s.mu.Unlock()
		if last {
			s.set(idle, true)
		}
	}
}

// Thinking accumulates reasoning deltas onto the current in-progress line exactly
// as Text does, so a child's chain-of-thought is surfaced rather than collapsed.
func (s *childSink) Thinking(delta string) { s.accumulate(delta, streamThinking) }

// Text accumulates streaming deltas onto the current in-progress line; see accumulate.
func (s *childSink) Text(delta string) { s.accumulate(delta, streamText) }

// accumulate appends delta to the current in-progress line for src, scrolling past
// completed newlines and publishing only its head; switching streams starts fresh.
func (s *childSink) accumulate(delta string, src stream) {
	s.mu.Lock()
	if s.src != src { // a thinking block ended / text began: show the new content
		s.buf = ""
		s.src = src
	}
	if delta != "" { // deltas arrive per token; append, capping the running line
		b := s.buf + delta
		// scroll per line: drop everything before the last newline so we show only
		// the current in-progress line (completed lines scroll past)
		if i := strings.LastIndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
		if len(b) > maxBuf { // keep only the head so a huge single line never jumps
			b = b[:maxBuf]
		}
		s.buf = b
	}
	if strings.TrimSpace(s.buf) == "" { // blank/whitespace-only line: nothing to show
		s.mu.Unlock()
		return
	}
	display := rowLine(s.id, s.buf)
	s.mu.Unlock()
	s.set(display, false)
}

// TurnEnd resets the streamed line between a child's turns without dropping the
// row: a job outlives its turns (run nudges a child that produced no summary), and
// Manager.spawn owns the clear at every terminal path.
func (s *childSink) TurnEnd(result agent.TurnResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = "" // next turn starts a fresh output line
	s.src = streamNone
	s.calls = 0 // an interrupted dispatch must not strand the row on a tool label
	s.text = thinkingRow(s.id)
	s.flushLocked(time.Now()) // drops any pending flush and publishes the idle row
}

// toolLabel names the running call, preferring a rich provided label and falling
// back to name + first argument so built-ins (whose labels are bare words like
// "read") still read as real work rather than one token.
func toolLabel(call agent.ToolCall, label string) string {
	if len(strings.Fields(label)) > 1 { // a rich provided label already names the work
		return oneLine(label)
	}
	lbl := call.Name
	if arg := strutil.FirstArgText(call.Input); arg != "" { // bare built-in labels: add its target
		lbl += " " + strutil.FirstLine(arg)
	}
	return oneLine(lbl)
}

// oneLine collapses newlines and runs of whitespace to single spaces.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// thinkingRow is the coalesced idle line for a job.
func thinkingRow(id string) string { return id + "  thinking…" }

// rowLine prefixes an activity label with the job id, matching the "<id>  <label>" shape.
func rowLine(id, text string) string { return id + "  " + oneLine(text) }
