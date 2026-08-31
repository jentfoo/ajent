package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threeToolCalls frames three parallel calls as distinct blocks in one message.
func threeToolCalls() []llm.Event {
	ev := make([]llm.Event, 0, 9)
	for i, name := range []string{"a", "b", "c"} {
		id := string(rune('1' + i))
		ev = append(ev,
			llm.Event{Type: llm.EventToolCallStart, Index: i, ToolCallID: id, ToolName: name},
			llm.Event{Type: llm.EventToolCallDelta, Index: i, Text: `{}`},
			llm.Event{Type: llm.EventToolCallEnd, Index: i,
				Block: llm.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
		)
	}
	return ev
}

// TestDispatchParallelAppendsResultsInCallOrder asserts three parallel tools
// produce results in the order of their calls regardless of completion timing.
func TestDispatchParallelAppendsResultsInCallOrder(t *testing.T) {
	t.Parallel()

	set := &mapSet{tools: map[string]Tool{
		"a": &stubTool{name: "a", result: "ra", parallel: true},
		"b": &stubTool{name: "b", result: "rb", parallel: true},
		"c": &stubTool{name: "c", result: "rc", parallel: true},
	}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(threeToolCalls(), doneEvent())},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	var ids []string
	for _, m := range a.state.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, blk := range m.Content {
			if tr, ok := blk.(llm.ToolResultBlock); ok {
				ids = append(ids, tr.CallID)
			}
		}
	}
	assert.Equal(t, []string{"1", "2", "3"}, ids) // call order preserved
}

// TestOnToolBatchSeesCallsInMessageOrder asserts the hook gets one step's calls
// in message order and runs before any of them: parallel dispatch races the calls
// against each other, so this is the only ordered view of a batch a host gets.
func TestOnToolBatchSeesCallsInMessageOrder(t *testing.T) {
	t.Parallel()

	stubs := map[string]*stubTool{
		"a": {name: "a", result: "ra", parallel: true},
		"b": {name: "b", result: "rb", parallel: true},
		"c": {name: "c", result: "rc", parallel: true},
	}
	set := &mapSet{tools: map[string]Tool{"a": stubs["a"], "b": stubs["b"], "c": stubs["c"]}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(threeToolCalls(), doneEvent())},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	var batches [][]string
	a.opts.OnToolBatch = func(_ context.Context, calls []ToolCall) {
		for name, st := range stubs {
			assert.Zero(t, st.callCount(), "%s ran before the hook saw the batch", name)
		}
		names := make([]string, len(calls))
		for i, c := range calls {
			names[i] = c.Name + ":" + c.ID
		}
		batches = append(batches, names)
	}

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
	assert.Equal(t, [][]string{{"a:1", "b:2", "c:3"}}, batches)
}

// diffTool emits a Diff through its output so the wiring to the sink is tested.
type diffTool struct{ stubTool }

func (t *diffTool) Execute(ctx context.Context, call ToolCall, out Output) (ToolResult, error) {
	out.Diff("file.go", "old", "new")
	return t.stubTool.Execute(ctx, call, out)
}

// TestDispatchDiffReachesSink asserts a tool's Diff lands on the sink.
func TestDispatchDiffReachesSink(t *testing.T) {
	t.Parallel()

	set := &mapSet{tools: map[string]Tool{"edit": &diffTool{stubTool{name: "edit", result: "ok"}}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "edit")},
		{Events: textOnly("done")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	assert.Contains(t, sink.calls, "diff") // the diff reached the sink
}

// TestToolStartFiresOncePerCall asserts ToolStart/done wrap each execution once.
func TestToolStartFiresOncePerCall(t *testing.T) {
	t.Parallel()

	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	count := 0
	for _, c := range sink.calls {
		if c == "tool_start:bash" {
			count++
		}
	}
	assert.Equal(t, 1, count) // ToolStart fires exactly once for the single call
}

// TestLoopMirrorsToolNamesAtTurnStart asserts state.Tools is populated from the
// enabled toolset when a turn begins.
func TestLoopMirrorsToolNamesAtTurnStart(t *testing.T) {
	t.Parallel()

	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	assert.Equal(t, []string{"bash"}, a.state.Tools) // enabled names mirrored for the transcript
}
