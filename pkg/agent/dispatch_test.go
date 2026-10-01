package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

// blockedCalls frames three tool-call blocks as a dispatch batch.
func blockedCalls() []llm.ToolCallBlock {
	return []llm.ToolCallBlock{
		{ID: "1", Name: "a", Input: json.RawMessage(`{}`)},
		{ID: "2", Name: "b", Input: json.RawMessage(`{}`)},
		{ID: "3", Name: "c", Input: json.RawMessage(`{}`)},
	}
}

// TestDispatchCancelledBatchSkipsLaunch asserts a cancelled turn context stops
// both dispatch paths from launching new calls, matching the serial loop's
// early break so an abort never runs the rest of a batch.
func TestDispatchCancelledBatchSkipsLaunch(t *testing.T) {
	t.Parallel()

	tools := map[string]Tool{
		"a": &stubTool{name: "a", result: "ra", parallel: true},
		"b": &stubTool{name: "b", result: "rb", parallel: true},
		"c": &stubTool{name: "c", result: "rc", parallel: true},
	}

	t.Run("parallel", func(t *testing.T) {
		set := &mapSet{tools: tools}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set
		a.state.Model.Caps.ParallelTools = true

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		a.dispatch(ctx, NopSink{}, blockedCalls())

		for _, name := range []string{"a", "b", "c"} {
			assert.Zero(t, set.tools[name].(*stubTool).callCount())
		}
	})

	t.Run("serial", func(t *testing.T) {
		set := &mapSet{tools: tools}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		a.dispatch(ctx, NopSink{}, blockedCalls())

		for _, name := range []string{"a", "b", "c"} {
			assert.Zero(t, set.tools[name].(*stubTool).callCount())
		}
	})
}

// ignoreCtxTool blocks on a channel without observing ctx, so an abort cannot
// release it and it keeps running through the interrupt.
type ignoreCtxTool struct {
	stubTool
	block chan struct{}
}

func (t *ignoreCtxTool) Execute(_ context.Context, call ToolCall, _ Output) (ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	<-t.block
	return ToolResult{Content: llm.BlockList{llm.TextBlock{Text: t.result}}}, nil
}

// errTool returns a canned result alongside its error, so runTool's fill rules
// can be exercised against non-empty content.
type errTool struct {
	stubTool
	res ToolResult
}

func (t *errTool) Execute(context.Context, ToolCall, Output) (ToolResult, error) {
	return t.res, t.err
}

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

// TestRunToolInterruptErrorFillsMarker asserts a tool that only propagates the
// cancellation reads as interrupted by user in the transcript, never raw
// context canceled, while real partial output is kept as-is.
func TestRunToolInterruptErrorFillsMarker(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("canceled_error_becomes_marker", func(t *testing.T) {
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", err: context.Canceled}}}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set

		res, _ := a.runTool(cancelled, NopSink{}, ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})
		require.True(t, res.IsError)
		tb := res.Content[0].(llm.TextBlock)
		assert.Equal(t, InterruptedText, tb.Text)
	})

	t.Run("clean_context_keeps_raw_error", func(t *testing.T) {
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", err: context.Canceled}}}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set

		res, _ := a.runTool(t.Context(), NopSink{}, ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})
		require.True(t, res.IsError)
		tb := res.Content[0].(llm.TextBlock)
		assert.Equal(t, context.Canceled.Error(), tb.Text)
	})

	t.Run("other_error_keeps_raw_text", func(t *testing.T) {
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", err: assert.AnError}}}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set

		res, _ := a.runTool(cancelled, NopSink{}, ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})
		require.True(t, res.IsError)
		tb := res.Content[0].(llm.TextBlock)
		assert.Equal(t, assert.AnError.Error(), tb.Text)
	})

	t.Run("real_partial_output_kept", func(t *testing.T) {
		tool := &errTool{stubTool: stubTool{name: "bash"},
			res: ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "partial output"}}}}
		tool.err = context.Canceled
		set := &mapSet{tools: map[string]Tool{"bash": tool}}
		a := newTestAgent(nil, nil, NopSink{})
		a.opts.Tools = set

		res, _ := a.runTool(cancelled, NopSink{}, ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})
		require.True(t, res.IsError)
		tb := res.Content[0].(llm.TextBlock)
		assert.Equal(t, "partial output", tb.Text)
	})
}

// TestDispatchAbortIgnoresToolContext pins the full abort path against a tool
// that keeps running through the interrupt: the turn still ends aborted with a
// well-formed transcript, and the late results stand over synthetic fills.
func TestDispatchAbortIgnoresToolContext(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{
		"a": &ignoreCtxTool{stubTool: stubTool{name: "a", result: "ra", parallel: true}, block: block},
		"b": &ignoreCtxTool{stubTool: stubTool{name: "b", result: "rb", parallel: true}, block: block},
	}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(twoToolCalls("c1", "a", "c2", "b"), doneEvent())},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool {
		return set.tools["a"].(*ignoreCtxTool).callCount() == 1 &&
			set.tools["b"].(*ignoreCtxTool).callCount() == 1
	}, defaultTimeout, pollInterval, "both tools must start before the interrupt")

	a.Interrupt()
	close(block) // the tools ignore ctx, so only release lets dispatch return

	require.NoError(t, <-errCh)
	assert.Equal(t, llm.StopAborted, catch.result.Stop)
	assert.True(t, wellFormed(a.state.Messages))
	var texts []string
	for _, m := range a.state.Messages {
		for _, blk := range m.Content {
			if tr, ok := blk.(llm.ToolResultBlock); ok {
				assert.False(t, tr.IsError) // the completed results stand, not synthetic fills
				for _, cb := range tr.Content {
					if tb, ok := cb.(llm.TextBlock); ok {
						texts = append(texts, tb.Text)
					}
				}
			}
		}
	}
	assert.Equal(t, []string{"ra", "rb"}, texts)
}

// diffTool emits a Diff through its output so the wiring to the sink is tested.
type diffTool struct{ stubTool }

func (t *diffTool) Execute(ctx context.Context, call ToolCall, out Output) (ToolResult, error) {
	out.Diff("file.go", "old", "new")
	return t.stubTool.Execute(ctx, call, out)
}

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

	var count int
	for _, c := range sink.calls {
		if c == "tool_start:bash" {
			count++
		}
	}
	assert.Equal(t, 1, count) // ToolStart fires exactly once for the single call
}

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
