package agent

import (
	"context"

	"github.com/jentfoo/ajent/pkg/llm"
)

// InjectPair runs tool as a synthetic call through sink and returns the call and
// result messages to place ahead of a user message, plus the raw result. Use it
// for host-initiated tool work that must read like the model's own — an injected
// @ reference, a project survey — so truncation, display and tracking follow the
// one path an agent-run tool takes.
func InjectPair(ctx context.Context, tool Tool, sink Sink, call ToolCall, label string) ([]llm.Message, ToolResult) {
	if tool == nil {
		return nil, ToolResult{}
	}
	if sink == nil {
		sink = NopSink{}
	}
	out := NewOutput(sink, call.ID)
	done := sink.ToolStart(call, label)
	res, err := tool.Execute(ctx, call, out)
	if res.Content == nil {
		res.Content = llm.BlockList{}
	}
	if err != nil && !res.IsError {
		res.IsError = true
		if len(res.Content) == 0 {
			res.Content = llm.BlockList{llm.TextBlock{Text: err.Error()}}
		}
	}
	if done != nil {
		done(res)
	}
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
			ID: call.ID, Name: call.Name, Input: call.Input}}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: call.ID, Content: res.Content, Display: res.Display, IsError: res.IsError}}},
	}, res
}
