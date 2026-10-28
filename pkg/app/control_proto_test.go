package app

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// TestControlStateGetSnapshot pins snapshot truth: every state.get field agrees
// with the hub's own snapshot on the same tick, and tool toggles surface in a
// later snapshot.
func TestControlStateGetSnapshot(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	ui := rig.hub.ui
	ui.SetStatusSegment(segment(segMCP, "mcp 2/2", "2/2"))
	ui.SetActivity("scan", "reading pkg")
	ui.SetQueued([]string{"next prompt"})

	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "ctl-test"})
	require.NoError(t, err)
	rig.hub.tools = reg

	var st ctl.State
	rp := c.call("a1", ctl.MethodState, map[string]any{})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&st))

	snap := rig.hub.Snapshot()
	assert.Equal(t, snap.SessionID, st.Session.ID)
	assert.Equal(t, "/repo", st.Workspace)
	assert.True(t, st.Idle)
	assert.False(t, st.Running)
	assert.Equal(t, []string{"next prompt"}, st.QueuedTexts)
	require.Len(t, st.Activity, 1)
	assert.Equal(t, ctl.ActivityRow{Key: "scan", Text: "reading pkg", Rank: activityUnranked}, st.Activity[0])
	require.Len(t, st.Status.Segments, len(snap.Status.Segments))
	assert.True(t, toolRow(t, st.Tools, tools.ToolRead).Enabled)
	assert.True(t, toolRow(t, st.Tools, tools.ToolRead).ReadOnly)

	// a /tools toggle shows up in the next snapshot: only the named set stays on
	reg.SetEnabled([]string{tools.ToolRead})
	rp = c.call("a2", ctl.MethodState, map[string]any{})
	require.NoError(t, rp.DecodeResult(&st))
	assert.True(t, toolRow(t, st.Tools, tools.ToolRead).Enabled)
	assert.False(t, toolRow(t, st.Tools, tools.ToolBash).Enabled)
}

func toolRow(t *testing.T, rows []ctl.ToolState, name string) ctl.ToolState {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("tool %s missing from snapshot", name)
	return ctl.ToolState{}
}

// TestControlUnknownMethodRefused pins the legibility rule: an unknown method
// errors by code, never a silent no-op.
func TestControlUnknownMethodRefused(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	rp := c.call("x1", "prompt.send", map[string]any{})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeUnknownMethod, rp.Error.Code)
	assert.Contains(t, rp.Error.Message, "prompt.send")
}

// TestControlWatermarkAppliesOnce pins the watermark rule: events at or below
// a reply's seq are superseded by the result, events above it apply exactly
// once, so an interleaved snapshot and stream reconcile with no second concept.
func TestControlWatermarkAppliesOnce(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	rig.hub.publish("marker", map[string]string{"v": "one"})
	rig.hub.publish("marker", map[string]string{"v": "two"})

	rp := c.call("w1", ctl.MethodState, map[string]any{})
	require.Nil(t, rp.Error)

	// everything the call waited through sits at or below the watermark
	waited := c.takePending()
	superseded := make([]uint64, 0, len(waited))
	for _, ev := range waited {
		require.Equal(t, "marker", ev.Name)
		assert.LessOrEqual(t, ev.Seq, rp.Seq)
		superseded = append(superseded, ev.Seq)
	}
	require.Len(t, superseded, 2)

	// an event past the watermark applies exactly once
	rig.hub.publish("marker", map[string]string{"v": "three"})
	ev := c.nextEvent()
	require.Equal(t, "marker", ev.Name)
	assert.Greater(t, ev.Seq, rp.Seq)
	var d struct {
		V string `json:"v"`
	}
	require.NoError(t, ev.Decode(&d))
	assert.Equal(t, "three", d.V)
}

// TestControlTranscriptBackfill pins the reconciliation source: entries match
// session.Read/Branch for the live head, the after cursor resumes without
// gaps, and entry.append events replayed against a snapshot reconstruct the
// branch exactly. Pre-attach history never announces.
func TestControlTranscriptBackfill(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "recorded before attach")
	})
	c := dialControl(t, rig.path)
	c.hello()

	writeMessage(t, rig.rec, "written after attach")

	var tr ctl.TranscriptResult
	rp := c.call("t1", ctl.MethodTranscript, map[string]any{})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&tr))

	entries, _, err := session.Read(rig.rec.w.Path())
	require.NoError(t, err)
	branch := session.Branch(entries, rig.rec.w.Head())
	require.Len(t, tr.Entries, len(branch))
	for i := range branch {
		assert.Equal(t, branch[i].ID, tr.Entries[i].ID)
		assert.Equal(t, branch[i].ParentID, tr.Entries[i].ParentID)
		assert.Equal(t, string(branch[i].Type), tr.Entries[i].Type)
		assert.Equal(t, branch[i].TS, tr.Entries[i].TS)
		assert.JSONEq(t, string(branch[i].Data), string(tr.Entries[i].Data))
	}
	assert.Equal(t, rig.rec.w.Head(), tr.Head)

	// exactly one append announced: the pre-attach entry never did
	appended := c.collect(ctl.EventEntryAppend, 1)
	var ed ctl.EntryData
	require.NoError(t, appended[0].Decode(&ed))
	assert.Equal(t, branch[len(branch)-1].ID, ed.Entry.ID)

	// the after cursor resumes with no gaps and no repeats
	mid := branch[1].ID
	rp = c.call("t2", ctl.MethodTranscript, map[string]any{"after": mid})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&tr))
	require.NotEmpty(t, tr.Entries)
	assert.Equal(t, mid, tr.Entries[0].ParentID)
	assert.NotContains(t, entryIDs(tr.Entries), mid)

	// an unknown cursor errors legibly rather than resuming wrong
	rp = c.call("t3", ctl.MethodTranscript, map[string]any{"after": "nope"})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
}

// TestControlTranscriptUnreadableRefused pins the refusal when the transcript
// cannot be read: a legible error, never an empty answer.
func TestControlTranscriptUnreadableRefused(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	require.NoError(t, os.Remove(rig.rec.w.Path()))
	c := dialControl(t, rig.path)
	c.hello()

	rp := c.call("t1", ctl.MethodTranscript, map[string]any{})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
}

func entryIDs(entries []ctl.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// writeMessage records one message entry on the transcript.
func writeMessage(t *testing.T, rec *sessRec, text string) {
	t.Helper()
	rec.rec.Message(agent.MessageInfo{
		Message: llm.Message{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: text}}},
	})
}

// TestControlEventsReplay pins reconnect support: events past a since seq
// return from the ring, anything older answers too-old and the client
// re-snapshots.
func TestControlEventsReplay(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()

	for range 4 {
		rig.hub.publish("marker", nil)
	}
	var rr ctl.ReplayResult
	rp := c.call("r1", ctl.MethodReplay, ctl.ReplayParams{Since: 2})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&rr))
	require.Len(t, rr.Events, 2)
	assert.Equal(t, uint64(3), rr.Events[0].Seq)
	assert.Equal(t, uint64(4), rr.Events[1].Seq)

	// a since at or past the present answers empty, never errors
	rp = c.call("r2", ctl.MethodReplay, ctl.ReplayParams{Since: 99})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&rr))
	assert.Empty(t, rr.Events)

	// overflow the ring: the trimmed history answers too-old. The fill runs in
	// the background while the client keeps reading, so only the intended
	// connection state changes, never this client's own buffers.
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		for range eventRingSize {
			rig.hub.publish("filler", nil)
		}
	}()
	c.drainUntil(func() bool { return hubRingLen(rig.hub) >= eventRingSize })
	<-filled
	rp = c.call("r3", ctl.MethodReplay, ctl.ReplayParams{Since: 0})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeTooOld, rp.Error.Code)

	_, ok := rig.hub.eventsSince(4) // first filler seq still retained
	require.True(t, ok)
}

func hubRingLen(h *sessionHub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.evRing)
}

// TestControlConcurrentObservers pins the many-connection contract: three
// observers each see the full event sequence in order and can snapshot
// independently.
func TestControlConcurrentObservers(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	clients := make([]*ctlClient, 0, 3)
	for range 3 {
		c := dialControl(t, rig.path)
		c.hello()
		clients = append(clients, c)
	}

	for i := range 5 {
		rig.hub.publish("marker", map[string]int{"i": i})
	}
	for _, c := range clients {
		rp := c.call("s", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error, "each observer snapshots independently")
	}

	for _, c := range clients {
		markers := c.collect("marker", 5)
		seqs := make([]uint64, 0, len(markers))
		for _, ev := range markers {
			seqs = append(seqs, ev.Seq)
		}
		assert.Equal(t, []uint64{1, 2, 3, 4, 5}, seqs)
	}
}

// TestControlBackpressureDropsSlowReader pins fail-open delivery: a controller
// that stops reading is dropped by its bounded queue while the turn completes
// normally.
func TestControlBackpressureDropsSlowReader(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)

	// the stalled controller: dialed, hello read, then nothing. Its socket
	// receive buffer and outbound queue fill while the turn streams.
	stalled, err := (&net.Dialer{}).DialContext(t.Context(), "unix", rig.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stalled.Close() })
	buf := make([]byte, 4096)
	_, _ = stalled.Read(buf) // the hello line, nothing after

	// a healthy turn drives the stream until the stalled reader overflows
	st := &agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}
	prov := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: bigOutputTurn()},
		{Events: textTurn("flooded and done")},
	}}
	rig.hub.ag = agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return prov, nil },
		Tools:    singleToolSet{tool: chattyTool{}},
		Sinks:    []agent.Sink{rig.hub.sink()},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	done := make(chan error, 1)
	go func() { done <- rig.hub.ag.Prompt(t.Context(), agent.Input{Text: "run"}) }()

	require.Eventually(t, func() bool {
		rig.srv.mu.Lock()
		defer rig.srv.mu.Unlock()
		return len(rig.srv.conns) == 0
	}, 15*time.Second, 10*time.Millisecond, "the stalled connection was never dropped")

	require.NoError(t, <-done) // the turn completes normally despite the stall
}

// bigOutputTurn scripts one tool turn; chattyTool's output does the flooding.
func bigOutputTurn() []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventToolCallStart, ToolCallID: "c1", ToolName: tools.ToolBash},
		{Type: llm.EventToolCallEnd, ToolCallID: "c1",
			Block: llm.ToolCallBlock{ID: "c1", Name: tools.ToolBash, Input: []byte(`{"cmd":"true"}`)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// chattyTool streams far more output than any buffer chain holds.
type chattyTool struct{}

func (chattyTool) Name() string                { return tools.ToolBash }
func (chattyTool) Label(agent.ToolCall) string { return "bash: chatter" }
func (chattyTool) Description() string         { return "test tool" }
func (chattyTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: tools.ToolBash} }
func (chattyTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (chattyTool) Close()                      {}

func (chattyTool) Execute(_ context.Context, _ agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	// 400 events past the kernel buffers overflow the 256-slot queue while the
	// transcript stays small enough for the turn to finish quickly
	chunk := strings.Repeat("y", 16*1024)
	for range 400 {
		_, _ = io.WriteString(out, chunk)
	}
	return agent.ToolResult{Display: "done"}, nil
}
