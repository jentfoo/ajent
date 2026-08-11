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
	Diff(path, before, after string)
	Usage(llm.Usage)
	// Context reports how full the next request will be, exact after a response
	// and estimated while one streams.
	Context(tokens.ContextState)
	Notice(msg string, level Level)
	TurnEnd(TurnResult)
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
func (NopSink) Diff(string, string, string) {}
func (NopSink) Usage(llm.Usage)             {}
func (NopSink) Context(tokens.ContextState) {}
func (NopSink) Notice(string, Level)        {}
func (NopSink) TurnEnd(TurnResult)          {}
