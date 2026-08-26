package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEnv is a fixed environment so system prompt tests are deterministic.
var testEnv = Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"}

func newTestAgent(state *State, p llm.Provider, sink Sink) *Agent {
	var sinks []Sink
	if sink != nil {
		sinks = []Sink{sink}
	}
	opts := Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    sinks,
		Env:      testEnv,
	}
	if state == nil {
		state = &State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
	}
	return New(state, opts)
}

// textEvents frames a reply the way providers do: start, deltas, end.
func textEvents(parts ...string) []llm.Event {
	out := make([]llm.Event, 1, len(parts)+2)
	out[0] = llm.Event{Type: llm.EventTextStart, Index: 0}
	for _, p := range parts {
		out = append(out, llm.Event{Type: llm.EventTextDelta, Index: 0, Text: p})
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
	}
	out = append(out, llm.Event{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: b.String()}})
	return out
}

func textOnly(parts ...string) []llm.Event {
	ev := textEvents(parts...)
	ev = append(ev, doneEvent())
	return ev
}

func doneEvent() llm.Event { return llm.Event{Type: llm.EventDone, StopReason: llm.StopEndTurn} }

// toolCallEvents builds a scripted turn that ends with one tool call.
func toolCallEvents(id, name string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallDelta, Index: 0, Text: `{"a":1}`},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: name, Input: json.RawMessage(`{"a":1}`)}},
		doneEvent(),
	}
}

// twoToolCalls frames two parallel calls as distinct blocks in one message.
func twoToolCalls(aID, aName, bID, bName string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: aID, ToolName: aName},
		{Type: llm.EventToolCallDelta, Index: 0, Text: `{"a":1}`},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: aID, Name: aName, Input: json.RawMessage(`{"a":1}`)}},
		{Type: llm.EventToolCallStart, Index: 1, ToolCallID: bID, ToolName: bName},
		{Type: llm.EventToolCallDelta, Index: 1, Text: `{"b":2}`},
		{Type: llm.EventToolCallEnd, Index: 1, Block: llm.ToolCallBlock{
			ID: bID, Name: bName, Input: json.RawMessage(`{"b":2}`)}},
	}
}

// stubTool is a Tool that records executions and returns canned results. A non-
// nil block channel holds Execute open until it is closed, so tests can pin the
// turn in flight.
type stubTool struct {
	name     string
	result   string
	err      error
	parallel bool
	block    chan struct{}

	mu    sync.Mutex
	calls []ToolCall
}

// callCount reports how many times the tool was executed, safely.
func (t *stubTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

func (t *stubTool) Name() string { return t.name }
func (t *stubTool) Label(ToolCall) string {
	if t.name != "" {
		return t.name + ": ..."
	}
	return t.name
}
func (t *stubTool) Description() string    { return "test tool" }
func (t *stubTool) Schema() llm.ToolSchema { return llm.ToolSchema{Name: t.name} }
func (t *stubTool) Mode() ExecutionMode {
	if t.parallel {
		return ModeParallel
	}
	return ModeSerial
}
func (t *stubTool) Execute(ctx context.Context, call ToolCall, _ Output) (ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	block := t.block
	err := t.err
	result := t.result
	t.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done(): // an interrupt cancels in-flight tools too
			return ToolResult{}, ctx.Err()
		}
	}
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: llm.BlockList{llm.TextBlock{Text: result}}}, nil
}

// mapSet is a simple in-memory tool set.
type mapSet struct {
	tools map[string]Tool
}

func (s *mapSet) Get(name string) (Tool, bool) { t, ok := s.tools[name]; return t, ok }
func (s *mapSet) Schemas() []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.Schema())
	}
	return out
}
func (s *mapSet) Names() []string {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestLoopTextOnly(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textOnly("hello ", "world")},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)

	err := a.Prompt(t.Context(), Input{Text: "hi"})
	require.NoError(t, err)

	assert.Equal(t, llm.StopEndTurn, catch.result.Stop)
	require.Len(t, a.state.Messages, 2) // user echo + assistant reply
	assistant := a.state.Messages[1]
	assert.Equal(t, llm.RoleAssistant, assistant.Role)
	var b strings.Builder
	for _, blk := range assistant.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	assert.Equal(t, "hello world", b.String())
}

func TestLoopOneToolCall(t *testing.T) {
	t.Parallel()

	tool := &stubTool{name: "bash", result: "ok"}
	set := &mapSet{tools: map[string]Tool{"bash": tool}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "run it"})
	require.NoError(t, err)

	assert.Equal(t, llm.StopEndTurn, catch.result.Stop)
	require.Len(t, tool.calls, 1)
	assert.Equal(t, "bash", tool.calls[0].Name)

	// user echo, assistant tool call, user result, assistant reply
	require.Len(t, a.state.Messages, 4)

	// the streamed assistant turn is stamped with its producing model and stop reason
	turn := a.state.Messages[1]
	assert.Equal(t, llm.RoleAssistant, turn.Role)
	if assert.NotNil(t, turn.Origin) {
		assert.Equal(t, "test", turn.Origin.Model)
	}
	assert.Equal(t, llm.StopEndTurn, turn.Stop) // the fixture's done event carries no tool stop

	resultMsg := a.state.Messages[2]
	assert.Equal(t, llm.RoleUser, resultMsg.Role)
	tb, ok := resultMsg.Content[0].(llm.ToolResultBlock)
	require.True(t, ok)
	assert.Equal(t, "c1", tb.CallID)
	assert.Equal(t, "bash", tb.ToolName) // set at the source for result-name providers
	assert.False(t, tb.IsError)
}

func TestLoopParallelCalls(t *testing.T) {
	t.Parallel()

	mk := func(name string) *stubTool { return &stubTool{name: name, result: name + ":r", parallel: true} }
	ta, tb := mk("a"), mk("b")
	set := &mapSet{tools: map[string]Tool{"a": ta, "b": tb}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(twoToolCalls("c1", "a", "c2", "b"), doneEvent())},
		// second model turn after both results
		{Events: textOnly("both")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	err := a.Prompt(t.Context(), Input{Text: "parallel"})
	require.NoError(t, err)

	// results must appear in call order c1 then c2 regardless of completion
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
	assert.Equal(t, []string{"c1", "c2"}, ids)
}

// TestLoopToolFailureContinues asserts failed or unknown tools yield IsError
// results, letting the loop continue rather than aborting.
func TestLoopToolFailureContinues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		set      map[string]Tool
		recovery string
	}{
		{"tool_errors", map[string]Tool{"bash": &stubTool{name: "bash", err: errors.New("boom")}}, "recovered"},
		// model calls a tool that is not in the set; loop must keep going
		{"unknown_tool", map[string]Tool{}, "ok"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
				{Events: toolCallEvents("c1", "bash")},
				{Events: textOnly(tc.recovery)},
			}}
			a := newTestAgent(nil, p, nil)
			a.opts.Tools = &mapSet{tools: tc.set}

			err := a.Prompt(t.Context(), Input{Text: "x"})
			require.NoError(t, err)

			resultMsg := a.state.Messages[2]
			tb := resultMsg.Content[0].(llm.ToolResultBlock)
			assert.True(t, tb.IsError) // an erroring tool is a result, not a failure

			// the loop must continue to a second model step with its recovery reply.
			require.Len(t, a.state.Messages, 4) // echo, call, error result, recovery
			tb2, ok := a.state.Messages[3].Content[0].(llm.TextBlock)
			require.True(t, ok)
			assert.Equal(t, tc.recovery, tb2.Text)
		})
	}
}

func TestLoopMaxSteps(t *testing.T) {
	t.Parallel()

	// a finite cap trips the loop, ending cleanly rather than as an error.
	t.Run("step_limit_trips", func(t *testing.T) {
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash"}}}
		// every turn ends with a tool call that produces no text; the loop spins
		var turns []llm.ScriptedTurn
		for i := 0; i < 5; i++ {
			turns = append(turns, llm.ScriptedTurn{Events: toolCallEvents("c", "bash")})
		}
		p := &llm.ScriptedProvider{Turns: turns}
		catch := &resultCatcher{}
		a := newTestAgent(nil, p, catch)
		a.opts.Tools = set
		a.opts.MaxSteps = 3 // small limit for the test

		err := a.Prompt(t.Context(), Input{Text: "x"})
		require.NoError(t, err) // hitting the limit ends cleanly, not as an error
		assert.NotEqual(t, llm.StopEndTurn, catch.result.Stop)
		assert.Equal(t, 3, catch.result.Steps)
	})

	// a zero MaxSteps (the default) never trips: the loop runs as many steps as the model keeps calling tools.
	t.Run("unlimited_by_default", func(t *testing.T) {
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash"}}}
		turns := make([]llm.ScriptedTurn, 0, 6)
		for i := 0; i < 5; i++ { // five tool-call steps would trip any small cap
			turns = append(turns, llm.ScriptedTurn{Events: toolCallEvents("c", "bash")})
		}
		turns = append(turns, llm.ScriptedTurn{Events: textOnly("done")})
		p := &llm.ScriptedProvider{Turns: turns}
		catch := &resultCatcher{}
		a := newTestAgent(nil, p, catch)
		a.opts.Tools = set // MaxSteps stays 0: unlimited

		err := a.Prompt(t.Context(), Input{Text: "x"})
		require.NoError(t, err)
		assert.Equal(t, llm.StopEndTurn, catch.result.Stop) // ran to the natural end
		assert.Equal(t, 6, catch.result.Steps)
	})
}

// TestLoopProviderErrorPropagates asserts a provider error returns from Prompt
// and lands in TurnResult, whether mid-stream or before any events.
func TestLoopProviderErrorPropagates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		turn func(error) llm.ScriptedTurn
	}{
		{"error_mid_stream", func(sentinel error) llm.ScriptedTurn {
			return llm.ScriptedTurn{Events: append(textEvents("partial"),
				llm.Event{Type: llm.EventDone, Err: sentinel})}
		}},
		{"error_before_events", func(sentinel error) llm.ScriptedTurn {
			return llm.ScriptedTurn{Err: sentinel}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := errors.New("wire failure")
			catch := &resultCatcher{}
			a := newTestAgent(nil,
				&llm.ScriptedProvider{Turns: []llm.ScriptedTurn{tc.turn(sentinel)}}, catch)

			err := a.Prompt(t.Context(), Input{Text: "x"})
			require.ErrorIs(t, err, sentinel)
			assert.Equal(t, sentinel, catch.result.Err)
		})
	}
}

// TestSinkOrder asserts the exact sink call sequence for a thinking+text turn.
func TestSinkOrderThinkingPrecedesText(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: []llm.Event{
			{Type: llm.EventThinkingStart, Index: 0},
			{Type: llm.EventThinkingDelta, Text: "hmm"},
			{Type: llm.EventThinkingEnd, Index: 0, Block: llm.ThinkingBlock{Text: "hmm"}},
			// answer text
			{Type: llm.EventTextStart, Index: 1},
			{Type: llm.EventTextDelta, Index: 1, Text: "answer"},
			{Type: llm.EventTextEnd, Index: 1, Block: llm.TextBlock{Text: "answer"}},
			doneEvent(),
		}},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	// EndThinking must precede the first Text delta
	tIdx, endTIdx, textIdx := -1, -1, -1
	for i, c := range sink.calls {
		switch c {
		case "thinking":
			if tIdx < 0 {
				tIdx = i
			}
		case "end_thinking":
			endTIdx = i
		case "text":
			if textIdx < 0 {
				textIdx = i
			}
		}
	}
	assert.Greater(t, endTIdx, tIdx)
	assert.Greater(t, textIdx, endTIdx)
}

func TestLoopFollowUpRunsAfterTurn(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")}, // turn one, blocked on the tool
		{Events: textOnly("after tool")},       // finishes turn one
		{Events: textOnly("reply two")},        // the follow-up's own reply
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Prompt(t.Context(), Input{Text: "one"})
	}()
	require.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval,
		"turn one must be in flight before the follow-up is queued")

	assert.True(t, a.FollowUp(Input{Text: "two"}))
	close(block) // let turn one finish; the loop then drains the follow-up

	require.NoError(t, <-errCh)
	// turn one: the tool-call assistant message and its closing reply; turn two:
	// the follow-up's own reply
	var replies []string
	for _, m := range a.state.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if tb, ok := b.(llm.TextBlock); ok && tb.Text != "" {
				replies = append(replies, tb.Text)
			}
		}
	}
	assert.Equal(t, []string{"after tool", "reply two"}, replies)
}

func TestLoopSteerInjectsAtBoundary(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")}, // first step calls a tool, blocked
		// the second model call sees the steered message injected at the boundary
		{Events: textOnly("after steer")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "start"}) }()
	require.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval,
		"the turn must be in flight before steering is accepted")

	assert.True(t, a.Steer(Input{Text: "steered!"}))
	// an injected steer surfaces live (no submission echo) while the typed one stays silent.
	assert.True(t, a.Steer(Input{Text: "Allowed with note: keep it", Injected: true}))
	close(block) // release the tool; the next step boundary drains the steers

	require.NoError(t, <-errCh)
	foundSteer := false
	for _, m := range a.state.Messages {
		if m.Role == llm.RoleUser && len(m.Content) > 0 {
			if tb, ok := m.Content[0].(llm.TextBlock); ok && strings.Contains(tb.Text, "steered!") {
				foundSteer = true
			}
		}
	}
	assert.True(t, foundSteer)
	var live []string
	for _, c := range sink.calls {
		if strings.HasPrefix(c, "user:") {
			live = append(live, c)
		}
	}
	assert.Equal(t, []string{"user:Allowed with note: keep it"}, live) // only the injected steer echoes
}

const (
	defaultTimeout = 2 * time.Second
	pollInterval   = time.Millisecond
)

// TestInterruptDuringOverflowCompaction interrupts while an overflow compaction's
// model call is running; the retry runs under the turn context so Interrupt stops it.
func TestInterruptDuringOverflowCompaction(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Err: llm.ErrContextOverflow},
	}}
	catch := &resultCatcher{}

	entered := make(chan struct{}, 1)
	a := New(&State{Model: llm.Model{ID: "test"}}, Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []Sink{catch},
		Env:      testEnv,
		Compact: func(ctx context.Context, r CompactReason) (bool, error) {
			if r != CompactOverflow {
				return false, nil // threshold boundary compaction is uninterruptible by design
			}
			entered <- struct{}{}
			<-ctx.Done() // the summariser call blocks until the interrupt
			return false, ctx.Err()
		},
	})

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, defaultTimeout, pollInterval, "the overflow compaction must start before the interrupt")

	a.Interrupt()

	select {
	case err := <-errCh:
		require.NoError(t, err) // an interrupted retry is a clean abort, not a failure
	case <-time.After(defaultTimeout):
		t.Fatal("Prompt did not return after the interrupt")
	}
	assert.Equal(t, llm.StopAborted, catch.result.Stop)
}

func TestBuildRequest(t *testing.T) {
	t.Parallel()

	// a reasoning model with a full context window and an unsupported level
	m := llm.Model{ID: "test", Provider: "p",
		ContextWindow: 200000, MaxOutput: 8000,
		Caps: llm.Capabilities{Dialect: llm.DialectOpenAICompletions, Reasoning: true}}
	st := &State{
		Model:     m,
		Reasoning: llm.ReasoningConfig{Level: llm.LevelMax}, // xhigh/max opt-in -> clamps
	}
	a := newTestAgent(st, nil, NopSink{})

	t.Run("clamps_level_and_sets_output_cap", func(t *testing.T) {
		req := a.buildRequest()
		assert.Equal(t, llm.LevelHigh, req.Reasoning.Level) // max clamps down to high
		assert.Positive(t, req.MaxTokens)
	})
	t.Run("max_tokens_respects_the_window", func(t *testing.T) {
		st2 := &State{
			Model:     m,
			Reasoning: llm.ReasoningConfig{Level: llm.LevelHigh},
		}
		a2 := newTestAgent(st2, nil, NopSink{})
		req := a2.buildRequest()
		assert.LessOrEqual(t, req.MaxTokens, 200000) // never exceeds the window
	})
	t.Run("nil_ledger_full_cap", func(t *testing.T) {
		st3 := &State{
			Model:     m,
			Reasoning: llm.ReasoningConfig{Level: llm.LevelHigh},
			Tokens:    nil, // no accounting configured
		}
		a3 := newTestAgent(st3, nil, NopSink{})
		req := a3.buildRequest()
		assert.Equal(t, llm.MaxOutputFor(m, 0), req.MaxTokens) // no used tokens -> full window cap
	})
}

// TestLoopToolProgressReachesSink asserts a call's streamed arguments are
// reported before it runs, and that the row is closed when the call completes.
func TestLoopToolProgressReachesSink(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("line\\n", 200) // escaped newlines, as JSON carries them
	args := `{"path":"notes.go","content":"` + body + `"}`
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: []llm.Event{
			{Type: llm.EventToolCallStart, Index: 0, ToolCallID: "c1", ToolName: "write"},
			{Type: llm.EventToolCallDelta, Index: 0, Text: args},
			{Type: llm.EventToolCallEnd, Index: 0, ToolCallID: "c1", ToolName: "write",
				Block: llm.ToolCallBlock{ID: "c1", Name: "write", Input: json.RawMessage(args)}},
			doneEvent(),
		}},
		{Events: textOnly("done")},
	}}
	rec := &recordingSink{}
	a := newTestAgent(nil, p, rec)
	a.opts.Tools = &mapSet{tools: map[string]Tool{"write": &stubTool{name: "write", result: "ok"}}}

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "hi"}))

	require.NotEmpty(t, rec.progress)
	first := rec.progress[0]
	assert.Equal(t, "write", first.Name)
	assert.False(t, first.Done)

	last := rec.progress[len(rec.progress)-1]
	assert.True(t, last.Done) // the row is closed when the call completes
	assert.Equal(t, "notes.go", last.Path)
	assert.Equal(t, 200, last.Lines)
	assert.Equal(t, len(args), last.Bytes)
}
