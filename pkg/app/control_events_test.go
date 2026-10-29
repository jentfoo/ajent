package app

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// streamCallSink records each sink call as its control event name, the parity
// reference for a scripted turn.
type streamCallSink struct {
	agent.NopSink
	calls []string
}

func (s *streamCallSink) log(name string) { s.calls = append(s.calls, name) }

func (s *streamCallSink) TurnStart(agent.TurnInfo)  { s.log(ctl.EventTurnStart) }
func (s *streamCallSink) UserPrompt(string)         { s.log(ctl.EventUserPrompt) }
func (s *streamCallSink) Thinking(string)           { s.log(ctl.EventThinkingDelta) }
func (s *streamCallSink) EndThinking()              { s.log(ctl.EventThinkingEnd) }
func (s *streamCallSink) Text(string)               { s.log(ctl.EventTextDelta) }
func (s *streamCallSink) EndText()                  { s.log(ctl.EventTextEnd) }
func (s *streamCallSink) ToolOutput(string, string) { s.log(ctl.EventToolOutput) }
func (s *streamCallSink) ToolProgress(agent.ToolProgress) {
	s.log(ctl.EventToolProgress)
}
func (s *streamCallSink) Diff(p, b, a string) { s.log(ctl.EventDiff) }
func (s *streamCallSink) Usage(llm.Usage)     { s.log(ctl.EventUsage) }
func (s *streamCallSink) Context(tokens.ContextState) {
	s.log(ctl.EventContext)
}
func (s *streamCallSink) Notice(string, agent.Level) { s.log(ctl.EventNotice) }
func (s *streamCallSink) TurnEnd(agent.TurnResult)   { s.log(ctl.EventTurnEnd) }
func (s *streamCallSink) ToolStart(agent.ToolCall, string, bool) func(agent.ToolResult) {
	s.log(ctl.EventToolStart)
	return func(agent.ToolResult) { s.log(ctl.EventToolResult) }
}

// TestControlStreamParity pins one-to-one stream parity: every agent sink call
// on a scripted turn surfaces as exactly one event, in order, seq-dense,
// tool.result included, with the json one-shot's shapes.
func TestControlStreamParity(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	calls := &streamCallSink{}
	st := &agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}
	prov := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: richToolTurn()},
		{Events: textTurn("all done")},
	}}
	// the driver's wiring: the recorder wraps the hub sink, the call logger
	// rides behind the hub fanout, and the tool's streamed output floods past
	rig.hub.ag = agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return prov, nil },
		Tools:    singleToolSet{tool: echoTool{out: "streamed output\n"}},
		Sinks:    []agent.Sink{rig.rec.rec.Sink(rig.hub.sink())},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	rig.hub.Register(calls)
	require.NoError(t, rig.hub.ag.Prompt(t.Context(), agent.Input{Text: "go"}))

	// every event the client saw, in arrival order: the stream family maps one
	// to one onto the sink calls, and the whole sequence is seq-dense
	all := c.collectEvents(t, len(calls.calls))
	var names []string
	for _, ev := range all {
		if streamEvent(ev.Name) {
			names = append(names, ev.Name)
		}
	}
	assert.Equal(t, calls.calls, names)
	for i := 1; i < len(all); i++ {
		assert.Equal(t, all[i-1].Seq+1, all[i].Seq)
	}

	// key payloads carry the oneshot shapes
	var start ctl.TurnStartData
	require.NoError(t, lastEvent(t, all, ctl.EventTurnStart).Decode(&start))
	var tr ctl.ToolResultData
	require.NoError(t, lastEvent(t, all, ctl.EventToolResult).Decode(&tr))
	assert.Equal(t, "c1", tr.ID)
	assert.Equal(t, tools.ToolBash, tr.Name)
	assert.Equal(t, "streamed output\n", tr.Output) // streamed fallback, Display unset
	assert.False(t, tr.Error)
	var end ctl.TurnEndData
	require.NoError(t, lastEvent(t, all, ctl.EventTurnEnd).Decode(&end))
	assert.Equal(t, llm.StopEndTurn.String(), end.Stop)
	assert.Equal(t, 2, end.Steps)
}

// lastEvent finds the last event named name in a collected slice.
func lastEvent(t *testing.T, evs []ctl.Event, name string) ctl.Event {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == name {
			return evs[i]
		}
	}
	t.Fatalf("no %s event in the collected sequence", name)
	return ctl.Event{}
}

// streamEvent reports whether name belongs to the agent-sink family.
func streamEvent(name string) bool {
	switch name {
	case ctl.EventTurnStart, ctl.EventUserPrompt,
		ctl.EventThinkingDelta, ctl.EventThinkingEnd,
		ctl.EventTextDelta, ctl.EventTextEnd,
		ctl.EventToolStart, ctl.EventToolOutput, ctl.EventToolProgress,
		ctl.EventToolResult, ctl.EventDiff, ctl.EventUsage, ctl.EventContext,
		ctl.EventNotice, ctl.EventTurnEnd:
		return true
	}
	return false
}

// buffered drains whatever already arrived without waiting.
func (c *ctlClient) buffered() []ctl.Event {
	var out []ctl.Event
	for {
		select {
		case in, ok := <-c.inc:
			if !ok {
				return out
			}
			if in.Event != nil {
				out = append(out, *in.Event)
			}
		default:
			return out
		}
	}
}

// collectEventsAny gathers arrived events of any family until n are held.
func (c *ctlClient) collectEventsAny(t *testing.T, n int) []ctl.Event {
	t.Helper()
	out := append(c.takePending(), c.buffered()...)
	for len(out) < n {
		out = append(out, c.nextEvent())
	}
	return out
}

// collectEvents gathers every event already arrived, plus more until n
// stream-family events are held, one ordered sequence.
func (c *ctlClient) collectEvents(t *testing.T, n int) []ctl.Event {
	t.Helper()
	out := append(c.takePending(), c.buffered()...)
	have := func() int {
		var stream int
		for _, ev := range out {
			if streamEvent(ev.Name) {
				stream++
			}
		}
		return stream
	}
	for have() < n {
		out = append(out, c.nextEvent())
	}
	return out
}

// richToolTurn scripts one turn carrying thinking, text and a tool call.
func richToolTurn() []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventThinkingStart, Index: 0},
		{Type: llm.EventThinkingDelta, Index: 0, Text: "hmm "},
		{Type: llm.EventThinkingEnd, Index: 0, Block: llm.TextBlock{Text: "hmm"}},
		{Type: llm.EventTextStart, Index: 1},
		{Type: llm.EventTextDelta, Index: 1, Text: "partial "},
		{Type: llm.EventTextEnd, Index: 1, Block: llm.TextBlock{Text: "partial"}},
		{Type: llm.EventToolCallStart, ToolCallID: "c1", ToolName: tools.ToolBash},
		{Type: llm.EventToolCallEnd, ToolCallID: "c1",
			Block: llm.ToolCallBlock{ID: "c1", Name: tools.ToolBash, Input: []byte(`{"cmd":"true"}`)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// echoTool streams its fixed output and reports a display-less result, the
// shape whose tool.result falls back to streamed output.
type echoTool struct{ out string }

func (echoTool) Name() string                { return tools.ToolBash }
func (echoTool) Label(agent.ToolCall) string { return "bash: true" }
func (echoTool) Description() string         { return "test tool" }
func (echoTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: tools.ToolBash} }
func (echoTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (echoTool) Close()                      {}

func (e echoTool) Execute(_ context.Context, _ agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	_, _ = io.WriteString(out, e.out)
	return agent.ToolResult{}, nil
}

// TestControlSurfaceParity pins surface parity: each rendered change the TUI
// commits surfaces exactly once with the same content.
func TestControlSurfaceParity(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	ui := rig.hub.ui
	ui.UserEcho("a prompt")
	ui.Print("# printed block")
	ui.Notify("plain notice", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "keyed notice", tui.LevelInfo)
	ui.SetStatusSegment(segment(segMCP, "mcp 2/2", "2/2"))
	ui.SetModelOf(llm.Model{Provider: "p", ID: "big", ContextWindow: 200000})
	ui.SetQueued([]string{"queued a", "queued b"})

	want := []string{
		ctl.EventEcho, ctl.EventPrint, ctl.EventNotice, ctl.EventNotice,
		ctl.EventStatus, ctl.EventStatus, ctl.EventQueued,
	}
	all := c.collectEventsAny(t, len(want))
	names := make([]string, 0, len(all))
	for _, ev := range all {
		names = append(names, ev.Name)
	}
	assert.Equal(t, want, names)

	var echo ctl.EchoData
	require.NoError(t, lastEvent(t, all, ctl.EventEcho).Decode(&echo))
	assert.Equal(t, "a prompt", echo.Text)

	var pr ctl.PrintData
	require.NoError(t, lastEvent(t, all, ctl.EventPrint).Decode(&pr))
	assert.Equal(t, "# printed block", pr.Markdown)

	var keyed ctl.NoticeData
	require.NoError(t, matchEvent(t, all, ctl.EventNotice, "keyed notice").Decode(&keyed))
	assert.Equal(t, "subagent", keyed.Key)
	assert.Equal(t, "info", keyed.Level)

	var sd ctl.StatusData
	require.NoError(t, matchEvent(t, all, ctl.EventStatus, segMCP).Decode(&sd))
	require.NotNil(t, sd.Segment)
	assert.Equal(t, segMCP, sd.Segment.Key)
	assert.Equal(t, "mcp 2/2", sd.Segment.Text)

	var md ctl.StatusData
	require.NoError(t, matchEvent(t, all, ctl.EventStatus, "p/big").Decode(&md))
	require.NotNil(t, md.Model)
	assert.Equal(t, "p/big", md.Model.Key)
	assert.Equal(t, 200000, md.Model.Window)

	var qd ctl.QueuedData
	require.NoError(t, lastEvent(t, all, ctl.EventQueued).Decode(&qd))
	assert.Equal(t, []string{"queued a", "queued b"}, qd.Texts)
}

// matchEvent finds the event named name whose payload contains want.
func matchEvent(t *testing.T, evs []ctl.Event, name, want string) ctl.Event {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == name && bytes.Contains(evs[i].Data, []byte(want)) {
			return evs[i]
		}
	}
	t.Fatalf("no %s event carrying %q", name, want)
	return ctl.Event{}
}

// TestControlInteractionAnnouncements pins the dialog contract: a barrier
// decision and a pick dialog each raise exactly one interaction.pending, a
// local answer raises one interaction.settled carrying the outcome, and secret
// inputs never echo their text.
func TestControlInteractionAnnouncements(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	local := &scriptedLocal{mode: tui.ModeAlt, picks: []int{1}, inputs: []string{"sk-local-answer"}}
	rig := newServeRig(t, local)
	c := dialControl(t, rig.path)
	c.hello()

	// a pick dialog opens, the local racer answers it, and the outcome settles
	picked := make(chan int, 1)
	go func() {
		i, perr := rig.broker.Pick(t.Context(), "pick one", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
		assert.NoError(t, perr)
		picked <- i
	}()

	pending := c.collect(ctl.EventInteractionPending, 1)[0]
	var pd ctl.InteractionPendingData
	require.NoError(t, pending.Decode(&pd))
	assert.Equal(t, uint64(1), pd.ID)
	assert.Equal(t, "pick", pd.Kind)
	assert.Equal(t, "pick one", pd.Prompt)
	assert.Equal(t, []string{"a", "b"}, pd.Options)

	assert.Equal(t, 1, <-picked) // the local answer won the race
	settled := c.collect(ctl.EventInteractionSettled, 1)[0]
	var sdd ctl.InteractionSettledData
	require.NoError(t, settled.Decode(&sdd))
	assert.Equal(t, pd.ID, sdd.ID)
	assert.True(t, sdd.Answered)
	assert.Equal(t, 1, sdd.Index)

	// a secret input announces without its text; the local answer reaches its
	// caller but never the settled outcome
	secret := make(chan string, 1)
	go func() {
		ans, _ := rig.broker.InputSecret(t.Context(), "api key", "sk-...")
		secret <- ans
	}()
	pending = c.collect(ctl.EventInteractionPending, 1)[0]
	require.NoError(t, pending.Decode(&pd))
	assert.Equal(t, "secret", pd.Kind)
	assert.Equal(t, "api key", pd.Prompt)
	assert.Equal(t, "sk-local-answer", <-secret)

	settled = c.collect(ctl.EventInteractionSettled, 1)[0]
	require.NoError(t, settled.Decode(&sdd))
	assert.True(t, sdd.Answered)
	assert.Empty(t, sdd.Text) // the secret never echoes
}

// TestControlBarrierHoldsForRemote pins the serve-mode hold: with no local
// surface, an approval dialog opens, announces and holds until a remote answer
// or a signal resolves it.
func TestControlBarrierHoldsForRemote(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	answered := make(chan permit.Dialog, 1)
	go func() {
		dlg, oerr := rig.broker.OpenDecision(tui.DecisionRequest{
			Prompt: "allow?", Options: []tui.Option{{Label: "Yes"}, {Label: "No"}},
		})
		assert.NoError(t, oerr)
		answered <- dlg
	}()

	pending := c.collect(ctl.EventInteractionPending, 1)[0]
	var pd ctl.InteractionPendingData
	require.NoError(t, pending.Decode(&pd))
	assert.Equal(t, "decision", pd.Kind)

	// the remote answer resolves what no local racer could
	require.NoError(t, rig.broker.Resolve(pd.ID, interactionAnswer{Index: 1}))
	dlg := <-answered
	idx, werr := dlg.Wait(t.Context())
	require.NoError(t, werr)
	assert.Equal(t, 1, idx)
	settled := c.collect(ctl.EventInteractionSettled, 1)[0]
	var sdd ctl.InteractionSettledData
	require.NoError(t, settled.Decode(&sdd))
	assert.True(t, sdd.Answered)
	assert.Equal(t, 1, sdd.Index)
	assert.Less(t, pending.Seq, settled.Seq)
}
