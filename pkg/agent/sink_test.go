package agent

import (
	"github.com/jentfoo/ajent/pkg/llm"
)

// recordingSink captures every sink call in order, for asserting exact
// sequences.
type recordingSink struct {
	calls []string
}

func (r *recordingSink) TurnStart(TurnInfo) { r.calls = append(r.calls, "turn_start") }
func (r *recordingSink) Thinking(string)    { r.calls = append(r.calls, "thinking") }
func (r *recordingSink) EndThinking()       { r.calls = append(r.calls, "end_thinking") }
func (r *recordingSink) Text(string)        { r.calls = append(r.calls, "text") }
func (r *recordingSink) EndText()           { r.calls = append(r.calls, "end_text") }
func (r *recordingSink) ToolStart(call ToolCall, _ string) func(ToolResult) {
	r.calls = append(r.calls, "tool_start:"+call.Name)
	return func(ToolResult) {}
}
func (r *recordingSink) ToolOutput(string, string) { r.calls = append(r.calls, "tool_output") }
func (r *recordingSink) Diff(string, string, string) {
	r.calls = append(r.calls, "diff")
}
func (r *recordingSink) Usage(llm.Usage) { r.calls = append(r.calls, "usage") }
func (r *recordingSink) Notice(msg string, level Level) {
	r.calls = append(r.calls, "notice:"+msg)
	_ = level
}
func (r *recordingSink) TurnEnd(TurnResult) { r.calls = append(r.calls, "turn_end") }

// lastTurn captures the final TurnResult a sink saw.
type resultCatcher struct {
	result TurnResult
}

func (c *resultCatcher) TurnStart(TurnInfo) {}
func (c *resultCatcher) Thinking(string)    {}
func (c *resultCatcher) EndThinking()       {}
func (c *resultCatcher) Text(string)        {}
func (c *resultCatcher) EndText()           {}
func (c *resultCatcher) ToolStart(ToolCall, string) func(ToolResult) {
	return func(ToolResult) {}
}
func (c *resultCatcher) ToolOutput(string, string)   {}
func (c *resultCatcher) Diff(string, string, string) {}
func (c *resultCatcher) Usage(llm.Usage)             {}
func (c *resultCatcher) Notice(string, Level)        {}
func (c *resultCatcher) TurnEnd(r TurnResult)        { c.result = r }
