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
var testEnv = Environment{Cwd: "/repo", OS: "linux/amd64", Shell: "bash", Date: "2024-01-02"}

func newTestAgent(state *State, p llm.Provider, sink Sink) *Agent {
	opts := Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sink:     sink,
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
	resultMsg := a.state.Messages[2]
	assert.Equal(t, llm.RoleUser, resultMsg.Role)
	tb, ok := resultMsg.Content[0].(llm.ToolResultBlock)
	require.True(t, ok)
	assert.Equal(t, "c1", tb.CallID)
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

func TestLoopToolErrorContinues(t *testing.T) {
	t.Parallel()

	tool := &stubTool{name: "bash", err: errors.New("boom")}
	set := &mapSet{tools: map[string]Tool{"bash": tool}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("recovered")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	resultMsg := a.state.Messages[2]
	tb := resultMsg.Content[0].(llm.ToolResultBlock)
	assert.True(t, tb.IsError) // an erroring tool is a result, not a failure
}

func TestLoopUnknownToolContinues(t *testing.T) {
	t.Parallel()

	// model calls a tool that is not in the set; loop must keep going
	set := &mapSet{tools: map[string]Tool{}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "ghost")},
		{Events: textOnly("ok")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	resultMsg := a.state.Messages[2]
	tb := resultMsg.Content[0].(llm.ToolResultBlock)
	assert.True(t, tb.IsError)
}

func TestLoopStepLimitTrips(t *testing.T) {
	t.Parallel()

	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash"}}}
	// every turn ends with a tool call that produces no text; the loop spins
	var turns []llm.ScriptedTurn
	for i := 0; i < defaultMaxSteps+1; i++ {
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
}

func TestLoopProviderErrorMidStream(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("wire failure")
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(textEvents("partial"), llm.Event{Type: llm.EventDone, Err: sentinel})},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, sentinel, catch.result.Err)
}

func TestLoopProviderStreamErrorBeforeEvents(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("stream refused")
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Err: sentinel}}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, sentinel, catch.result.Err)
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
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "start"}) }()
	require.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval,
		"the turn must be in flight before steering is accepted")

	assert.True(t, a.Steer(Input{Text: "steered!"}))
	close(block) // release the tool; the next step boundary drains the steer

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
}

const (
	defaultTimeout = 2 * time.Second
	pollInterval   = time.Millisecond
)
