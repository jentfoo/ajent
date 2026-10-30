package agent

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// Level is the severity of a notice.
type Level uint8

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Sink receives every event a turn produces. The interactive TUI adapts it onto
// tui.UI. A headless caller can supply a JSON writer and a sub-agent a null sink.
type Sink interface {
	TurnStart(TurnInfo)
	// UserPrompt echoes a prompt's words. Live sessions echo at submission time,
	// replay calls this so restored context shows each user message above its reply.
	UserPrompt(text string)
	// InjectContext paints committed injected context where it lands, in the
	// block shape InjectBlock defines. Steering notes and sub-agent notices
	// echo through it, so their live rendering matches what replay draws for
	// the same entry on every surface.
	InjectContext(text string)
	Thinking(delta string)
	EndThinking()
	Text(delta string)
	EndText()
	// ToolStart opens a tool call's header and returns its completion callback,
	// invoked exactly once by the loop with the final result, so it must be
	// non-nil. full marks a call whose streamed output shows in full rather
	// than collapsed to its head.
	ToolStart(call ToolCall, label string, full bool) func(ToolResult)
	ToolOutput(callID, delta string)
	// ToolProgress reports a call the model is still streaming, so a long set of
	// arguments shows movement before the call can run.
	ToolProgress(ToolProgress)
	Diff(path, before, after string)
	Usage(llm.Usage)
	// Context reports how full the next request will be, exact after a response
	// and estimated while one streams.
	Context(tokens.ContextState)
	Notice(msg string, level Level)
	TurnEnd(TurnResult)
}

// fanoutSink forwards every event to each member in registration order, so more
// than one consumer can watch turns without displacing the recorder.
type fanoutSink struct {
	sinks []Sink
}

func (f *fanoutSink) TurnStart(i TurnInfo) {
	for _, s := range f.sinks {
		s.TurnStart(i)
	}
}

// InjectBlock paints one committed injected context block: first line as its
// header label, remainder collapsed beneath it. The single shape for
// replay-marked injected content, shared by the live steering echo and session
// replay, so a note renders identically on every surface.
func InjectBlock(id string, text string, sink Sink) {
	label, body, _ := strings.Cut(text, "\n")
	if strings.TrimSpace(label) == "" {
		return
	}
	body = strings.TrimLeft(body, "\n") // the blank line separating label from body
	sink.ToolStart(ToolCall{ID: id}, label, false)(ToolResult{Display: body})
}
func (f *fanoutSink) UserPrompt(text string) {
	for _, s := range f.sinks {
		s.UserPrompt(text)
	}
}

func (f *fanoutSink) InjectContext(text string) {
	for _, s := range f.sinks {
		s.InjectContext(text)
	}
}
func (f *fanoutSink) Thinking(d string) {
	for _, s := range f.sinks {
		s.Thinking(d)
	}
}
func (f *fanoutSink) EndThinking() {
	for _, s := range f.sinks {
		s.EndThinking()
	}
}
func (f *fanoutSink) Text(d string) {
	for _, s := range f.sinks {
		s.Text(d)
	}
}
func (f *fanoutSink) EndText() {
	for _, s := range f.sinks {
		s.EndText()
	}
}

// ToolStart returns a closure that calls every member's done in order.
func (f *fanoutSink) ToolStart(call ToolCall, label string, full bool) func(ToolResult) {
	done := make([]func(ToolResult), 0, len(f.sinks))
	for _, s := range f.sinks {
		if d := s.ToolStart(call, label, full); d != nil {
			done = append(done, d)
		}
	}
	return func(r ToolResult) {
		for _, d := range done {
			d(r)
		}
	}
}

func (f *fanoutSink) ToolOutput(id, d string) {
	for _, s := range f.sinks {
		s.ToolOutput(id, d)
	}
}
func (f *fanoutSink) ToolProgress(p ToolProgress) {
	for _, s := range f.sinks {
		s.ToolProgress(p)
	}
}
func (f *fanoutSink) Diff(p, b, a string) {
	for _, s := range f.sinks {
		s.Diff(p, b, a)
	}
}
func (f *fanoutSink) Usage(u llm.Usage) {
	for _, s := range f.sinks {
		s.Usage(u)
	}
}
func (f *fanoutSink) Context(c tokens.ContextState) {
	for _, s := range f.sinks {
		s.Context(c)
	}
}
func (f *fanoutSink) Notice(msg string, level Level) {
	for _, s := range f.sinks {
		s.Notice(msg, level)
	}
}
func (f *fanoutSink) TurnEnd(r TurnResult) {
	for _, s := range f.sinks {
		s.TurnEnd(r)
	}
}

// NopSink discards every event, for a sub-agent with no UI.
type NopSink struct{}

func (NopSink) TurnStart(TurnInfo)   {}
func (NopSink) UserPrompt(string)    {}
func (NopSink) InjectContext(string) {}
func (NopSink) Thinking(string)      {}
func (NopSink) EndThinking()         {}
func (NopSink) Text(string)          {}
func (NopSink) EndText()             {}
func (NopSink) ToolStart(call ToolCall, label string, full bool) func(ToolResult) {
	return func(ToolResult) {}
}
func (NopSink) ToolOutput(string, string)   {}
func (NopSink) ToolProgress(ToolProgress)   {}
func (NopSink) Diff(string, string, string) {}
func (NopSink) Usage(llm.Usage)             {}
func (NopSink) Context(tokens.ContextState) {}
func (NopSink) Notice(string, Level)        {}
func (NopSink) TurnEnd(TurnResult)          {}
