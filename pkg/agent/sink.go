package agent

import (
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
// tui.UI; a headless caller can supply a JSON writer and a sub-agent a null sink.
type Sink interface {
	TurnStart(TurnInfo)
	Thinking(delta string)
	EndThinking()
	Text(delta string)
	EndText()
	ToolStart(call ToolCall, label string) func(ToolResult)
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
func (f *fanoutSink) ToolStart(call ToolCall, label string) func(ToolResult) {
	done := make([]func(ToolResult), 0, len(f.sinks))
	for _, s := range f.sinks {
		if d := s.ToolStart(call, label); d != nil {
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

func (NopSink) TurnStart(TurnInfo) {}
func (NopSink) Thinking(string)    {}
func (NopSink) EndThinking()       {}
func (NopSink) Text(string)        {}
func (NopSink) EndText()           {}
func (NopSink) ToolStart(call ToolCall, label string) func(ToolResult) {
	return func(ToolResult) {}
}
func (NopSink) ToolOutput(string, string)   {}
func (NopSink) ToolProgress(ToolProgress)   {}
func (NopSink) Diff(string, string, string) {}
func (NopSink) Usage(llm.Usage)             {}
func (NopSink) Context(tokens.ContextState) {}
func (NopSink) Notice(string, Level)        {}
func (NopSink) TurnEnd(TurnResult)          {}
