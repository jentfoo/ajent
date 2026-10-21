package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/plan"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

var (
	e2ePlanner     = llm.Model{Provider: "p", ID: "planner", ContextWindow: 100000}
	e2eImplementor = llm.Model{Provider: "p", ID: "implementor", ContextWindow: 100000}
)

// devCallTurn scripts a turn whose only output is one control-tool call.
func devCallTurn(id, name, args string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: name, Input: json.RawMessage(args)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// textTurn scripts a turn that only speaks, as an implementor that stops without calling dev_review does.
func textTurn(text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}
}

// planHarness is a live agent, transcript and tool registry driven by a real
// plan.Controller, so a test exercises branching and scoping rather than stubs.
type planHarness struct {
	ctl      *plan.Controller
	ag       *agent.Agent
	st       *agent.State
	rec      *sessRec
	toolsReg *tools.Registry
	expander *refs.Expander
	ui       *tui.UI
	reg      *llm.Registry
	dir      string
	planner  *llm.ScriptedProvider
	impl     *llm.ScriptedProvider
	editor   []string
	notices  []string
}

func TestForkToRefusedMidTurn(t *testing.T) {
	t.Parallel()

	ui, _, out := plainUI(t)
	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-plan.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "goal")})
	require.NoError(t, err)
	before := readEntriesRewind(t, p)

	m, err := reg.Resolve("p/a")
	require.NoError(t, err)
	ag, _, release := holdTurn(t)

	err = r.forkTo(ui, ag, reg, before[1].ID, m)
	release()

	require.ErrorIs(t, err, errTurnRunning)
	assert.Equal(t, before[len(before)-1].ID, w.Head()) // the head never moved
	assert.Len(t, readEntriesRewind(t, p), len(before)) // no model change recorded
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "plan: a turn is running")
	}, 2*time.Second, 5*time.Millisecond)
}

func TestForkToRefusedAfterSwitch(t *testing.T) {
	t.Parallel()

	ui, _, out := plainUI(t)
	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-plan.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}

	m, err := reg.Resolve("p/a")
	require.NoError(t, err)
	ag, _, start, release := heldTurnAgent(t)
	// onSwitch fires between the head move and the restore: land the turn there
	r.onSwitch = func([]llm.Message) { start() }

	err = r.forkTo(ui, ag, reg, "", m) // empty head: the implementation round's new root
	release()

	require.ErrorIs(t, err, errTurnRunning)
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "plan: model p/a recorded but not applied")
	}, 2*time.Second, 5*time.Millisecond)

	// the moved head owns the new root, whose first entry is the model change
	entries := readEntriesRewind(t, p)
	branch := session.Branch(entries, w.Head())
	require.NotEmpty(t, branch)
	assert.Equal(t, session.TypeModelChange, branch[0].Type)
	assert.Empty(t, branch[0].ParentID)
	var data session.ModelData
	require.NoError(t, branch[0].Decode(&data))
	assert.Equal(t, "p/a", data.Model)

	// the live model is whatever the turn runs with, not the recorded one
	var live llm.Model
	ag.WithState(func(s *agent.State) { live = s.Model })
	assert.Equal(t, "m", live.ID)

	// the branch still resolves its model once the turn ends
	rebuilt, _ := r.stateFor(modelResolver(reg), entries, w.Head())
	assert.Equal(t, "p/a", rebuilt.Model.Key())
}

// newPlanHarness wires the workflow over a real session and agent. Only the
// picker and the UI-facing seams are stubbed. Fork is the production path.
func newPlanHarness(t *testing.T, plannerTurns, implTurns []llm.ScriptedTurn) *planHarness {
	t.Helper()

	dir := t.TempDir()
	w, err := session.Create(filepath.Join(dir, "2026-01-02T03-04-05Z-plan.jsonl"),
		session.SessionData{Version: session.Version(), Model: e2eImplementor.Key()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	rec := &sessRec{store: session.StoreAt(dir), w: w, rec: session.NewRecorder(w)}

	inR, _, err := os.Pipe()
	require.NoError(t, err)
	_, outW, err := os.Pipe()
	require.NoError(t, err)
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	toolsReg, err := tools.Builtins(tools.Options{Cwd: dir})
	require.NoError(t, err)

	reg, warns := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	assert.Empty(t, warns)
	st := &agent.State{Model: e2eImplementor, Tokens: tokens.New(e2eImplementor)}

	h := &planHarness{
		st: st, rec: rec, toolsReg: toolsReg, dir: dir, ui: ui, reg: reg,
		expander: refs.NewExpander(toolsReg, agent.NopSink{}, tools.PathPolicy{Cwd: dir}, nil),
		planner:  &llm.ScriptedProvider{ProviderName: "planner", Turns: plannerTurns},
		impl:     &llm.ScriptedProvider{ProviderName: "impl", Turns: implTurns},
	}

	h.ag = agent.New(st, agent.Options{
		Provider: func(m llm.Model) (llm.Provider, error) {
			if m.ID == e2ePlanner.ID {
				return h.planner, nil
			}
			return h.impl, nil
		},
		Sinks:     []agent.Sink{rec.rec.Sink(agent.NopSink{})},
		OnMessage: []func(agent.MessageInfo){rec.rec.Message},
		Tools:     toolsReg,
		Env:       agent.Environment{Cwd: dir, OS: "linux/amd64", Date: "2024-01-02"},
		// the driver's real append-point seam, so kickoffs face the same
		// @-expansion pass a steered input does
		NormalizeInput: func(in agent.Input) agent.Input {
			return refs.Normalize(h.expander, in, func(string) {})
		},
	})

	h.ctl = plan.New(h.host())
	return h
}

// host wires the workflow over the live harness seams. A resumed process builds
// a fresh controller over the same surfaces.
func (h *planHarness) host() plan.Host {
	w, rec, st, toolsReg := h.rec.w, h.rec, h.st, h.toolsReg
	ui, reg := h.ui, h.reg
	return plan.Host{
		PickModel:   func(context.Context, string) (llm.Model, bool) { return e2ePlanner, true },
		ActiveModel: func() llm.Model { return st.Model },
		Running:     h.ag.Running,
		Abort:       h.ag.Interrupt,

		ToolNames: toolsReg.Names,
		// the planner may delegate read-only research to the enabled sub-agent tools
		PlannerTools: func() []string {
			all := bulk.SliceToSet(toolsReg.AllNames(tools.SourceBuiltin))
			return bulk.SliceFilterInPlace(func(n string) bool { _, ok := all[n]; return ok },
				[]string{subAgentToolStart, subAgentToolPoll, subAgentToolList})
		},
		SetTools: toolsReg.SetEnabled,
		AddTools: func(ts []agent.Tool) {
			for _, tool := range ts {
				toolsReg.RegisterFrom(plan.Source, tool, true)
			}
		},
		DropTools: func() { toolsReg.Unregister(plan.Source) },

		Fork: func(head string, m llm.Model) error { return rec.forkTo(ui, h.ag, reg, head, m) },
		Head: w.Head,

		Persist: func(v any) error { return rec.rec.Custom(plan.CustomType, v) },
		Restore: func(v any) bool {
			branch, ok := resumedBranch(w)
			if !ok {
				return false
			}
			return session.LatestCustom(branch, plan.CustomType, v)
		},
		Landed: func() bool {
			branch, ok := resumedBranch(w)
			return !ok || session.CustomLanded(branch, plan.CustomType)
		},
		ResolveModel: h.resolveModel,

		Messages: func() []llm.Message { return st.Messages },
		SetInput: func(s string) { h.editor = append(h.editor, s) },
		Notify:   func(msg string, _ agent.Level) { h.notices = append(h.notices, msg) },
		Ask: func(ctx context.Context, q string, opts []string) (int, error) {
			options := make([]tui.Option, len(opts))
			for i, o := range opts {
				options[i] = tui.Option{Label: o}
			}
			return ui.SelectContext(ctx, q, options)
		},
		Status: func(text, short string) {
			ui.SetStatusSegment(segment(segPlan, text, short))
		},
		Git: func(context.Context) (string, string) { return " M main.go", " main.go | 2 +-" },
	}
}

// resolveModel maps the scripted workflow roles onto their models.
func (h *planHarness) resolveModel(key string) (llm.Model, bool) {
	switch key {
	case e2ePlanner.Key():
		return e2ePlanner, true
	case e2eImplementor.Key():
		return e2eImplementor, true
	}
	return llm.Model{}, false
}

// prompt runs one turn through the pump and drain seams, expanding the typed
// line the way the pump does, and follows every turn the workflow takes over.
func (h *planHarness) prompt(t *testing.T, text string) {
	t.Helper()

	res := h.expander.Expand(text)
	in := agent.Input{Text: res.Text, After: res.Run, Prepared: true}
	if wrapped, ok := h.ctl.BeforePrompt(t.Context(), in); ok {
		in = wrapped
	}
	for {
		err := h.ag.Prompt(t.Context(), in)
		require.NoError(t, err)
		next, ok := h.ctl.Advance(t.Context(), agent.TurnResult{Stop: llm.StopEndTurn})
		if !ok {
			return
		}
		in = next
	}
}

// crashAt persists the workflow state BeforePrompt produced without running the
// turn it framed, the window a crash between persistence and delivery leaves.
func (h *planHarness) crashAt(t *testing.T, text string) {
	t.Helper()

	res := h.expander.Expand(text)
	_, ok := h.ctl.BeforePrompt(t.Context(), agent.Input{Text: res.Text, After: res.Run, Prepared: true})
	require.True(t, ok) // the transition persisted
}

// resumeFromTranscript simulates a fresh process over the crashed session: state
// rebuilds from the file, the workflow restores from the resumed branch. It
// returns what that branch actually carries.
func (h *planHarness) resumeFromTranscript(t *testing.T) []llm.Message {
	t.Helper()

	// the resumed process registers its own control tools
	h.toolsReg.Unregister(plan.Source)
	branch, ok := resumedBranch(h.rec.w)
	require.True(t, ok)
	rebuilt, _ := session.State(branch, func(key string) (llm.Model, error) {
		m, ok := h.resolveModel(key)
		if !ok {
			return m, errors.New("unresolved model: " + key)
		}
		return m, nil
	})
	h.st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		h.st.Model = rebuilt.Model
	}
	h.ctl = plan.New(h.host())
	require.True(t, h.ctl.Restore())
	return rebuilt.Messages
}

// texts flattens every text block a request carried, for content assertions.
func texts(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, blk := range m.Content {
			if tb, ok := blk.(llm.TextBlock); ok {
				b.WriteString(tb.Text + "\n")
			}
		}
	}
	return b.String()
}

// hasRefInjection reports whether a request carries an @-injected read/ls pair,
// told apart from model-requested calls by the ref- call-id prefix.
func hasRefInjection(req llm.Request) bool {
	return slices.ContainsFunc(req.Messages, func(m llm.Message) bool {
		return slices.ContainsFunc(m.Content, func(b llm.Block) bool {
			call, ok := b.(llm.ToolCallBlock)
			return ok && strings.HasPrefix(call.ID, "ref-")
		})
	})
}

// TestPlanWorkflowEndToEnd drives plan → user approval → implement → review →
// complete against two scripted providers, asserting which model ran in each
// phase and that the implementor's context holds the approved plan and nothing
// from the planning conversation.
func TestPlanWorkflowEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"drafted plan"}`)},
			{Events: devCallTurn("2", plan.DevCompleteTool, `{}`)},
		},
		[]llm.ScriptedTurn{
			{Events: devCallTurn("3", plan.DevReviewTool, `{"summary":"added the flag"}`)},
		},
	)

	require.Empty(t, h.ctl.Start(t.Context(), ""))
	assert.Equal(t, e2ePlanner, h.st.Model)

	// planning: the planner drafts and hands the plan to the user
	h.prompt(t, "add a --version flag")
	require.Equal(t, []string{"drafted plan"}, h.editor)
	assert.Contains(t, h.ctl.Status(), "awaiting plan")

	// the user edits before submitting, that text is the plan of record
	h.prompt(t, "edited plan")

	require.Len(t, h.impl.Requests(), 1)
	implCtx := texts(h.impl.Requests()[0])
	assert.Contains(t, implCtx, "edited plan")
	assert.NotContains(t, implCtx, "drafted plan")
	assert.NotContains(t, implCtx, "add a --version flag") // the goal never leaks
	assert.NotContains(t, implCtx, "planning, not implementing")

	// review ran on the planner and saw the approved plan plus the tree state
	require.Len(t, h.planner.Requests(), 2)
	reviewCtx := texts(h.planner.Requests()[1])
	assert.Contains(t, reviewCtx, "edited plan")
	assert.Contains(t, reviewCtx, "M main.go")
	assert.Contains(t, reviewCtx, "added the flag")
	assert.Contains(t, reviewCtx, "add a --version flag") // the reviewer keeps the goal

	assert.False(t, h.ctl.Active())
	assert.Equal(t, e2eImplementor, h.st.Model) // the original model is restored
	assert.Equal(t, []string{"read", "write", "edit", "bash"}, h.toolsReg.Names())
	assert.Empty(t, h.toolsReg.AllNames(plan.Source))
}

func TestPlanWorkflowBranchesPerPhase(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"the plan"}`)},
			{Events: devCallTurn("2", plan.DevCompleteTool, `{}`)},
		},
		[]llm.ScriptedTurn{
			{Events: textTurn("I edited main.go and added the flag")},
		},
	)
	require.Empty(t, h.ctl.Start(t.Context(), ""))
	h.prompt(t, "add a --version flag")
	h.prompt(t, "the plan")

	require.Len(t, h.planner.Requests(), 2)
	reviewCtx := texts(h.planner.Requests()[1])
	assert.Contains(t, reviewCtx, "I edited main.go and added the flag")

	entries, _, err := session.Read(h.rec.w.Path())
	require.NoError(t, err)

	// two roots: the session line, plus the independent implementation round.
	assert.Len(t, bulk.SliceFilter(func(e session.Entry) bool { return e.ParentID == "" }, entries), 2)
}

// TestPlanKickoffsStayUnexpanded drives the workflow over the driver's real
// @-expansion seam with @ paths in every authored surface: the gate submit's
// planned reads must not land behind the implementation kickoff, and paths
// quoted in summaries or revision instructions must stay literal pointers.
func TestPlanKickoffsStayUnexpanded(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"draft"}`)},
			{Events: devCallTurn("2", plan.DevReviseTool, `{"instructions":"also fix @note.go"}`)},
			{Events: devCallTurn("3", plan.DevCompleteTool, `{}`)},
		},
		[]llm.ScriptedTurn{
			{Events: devCallTurn("10", plan.DevReviewTool, `{"summary":"edited @flag.go, see @other.go"}`)},
			{Events: devCallTurn("11", plan.DevReviewTool, `{"summary":"done"}`)},
		},
	)
	// each quoted path is read by no earlier turn, so tracker dedupe cannot mask
	// an expansion: one for the gate submit, the summary and the instructions
	for name, body := range map[string]string{
		"flag.go":  "package main\n\nvar Flag = true\n",
		"other.go": "package main\n\nvar Other = 2\n",
		"note.go":  "package main\n\nvar Note = 3\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(h.dir, name), []byte(body), 0o644))
	}

	require.Empty(t, h.ctl.Start(t.Context(), ""))
	h.prompt(t, "add a flag")
	h.prompt(t, "edit @flag.go") // the gate captures a submit with a planned read

	// the whole workflow ran: two implementation rounds, a review after each
	require.Len(t, h.impl.Requests(), 2)
	require.Len(t, h.planner.Requests(), 3)

	// round 1: the fresh root holds the kickoff alone, the planned read never lands
	assert.Contains(t, texts(h.impl.Requests()[0]), "edit @flag.go")
	assert.NotContains(t, texts(h.impl.Requests()[0]), "var Flag = true")
	assert.False(t, hasRefInjection(h.impl.Requests()[0]))

	// review round 1: the quoted summary stays verbatim, no read pinned to it
	assert.Contains(t, texts(h.planner.Requests()[1]), "see @other.go")
	assert.NotContains(t, texts(h.planner.Requests()[1]), "var Other = 2")
	assert.False(t, hasRefInjection(h.planner.Requests()[1]))

	// round 2 kickoff: revision instructions ride in as literal pointers too
	assert.Contains(t, texts(h.impl.Requests()[1]), "also fix @note.go")
	assert.False(t, hasRefInjection(h.impl.Requests()[1]))

	assert.False(t, h.ctl.Active())
}

// TestPlanResumeRepairsMissingKickoff reproduces the crash window between the
// implementing transition persisting and its kickoff landing: the resumed root
// carries only bookkeeping entries, so the next prompt must re-deliver the
// kickoff ahead of itself rather than run the implementor context-free.
func TestPlanResumeRepairsMissingKickoff(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"the plan"}`)},
			{Events: devCallTurn("2", plan.DevCompleteTool, `{}`)},
		},
		[]llm.ScriptedTurn{
			{Events: devCallTurn("3", plan.DevReviewTool, `{"summary":"added the flag"}`)},
		},
	)
	require.Empty(t, h.ctl.Start(t.Context(), ""))
	h.prompt(t, "add a --version flag")
	require.Equal(t, []string{"the plan"}, h.editor)

	h.crashAt(t, "the plan") // approval persisted, the kickoff turn never ran

	msgs := h.resumeFromTranscript(t)
	require.Empty(t, msgs) // the new root holds no kickoff, only bookkeeping
	require.True(t, h.ctl.Active())

	h.prompt(t, "keep going")
	require.Len(t, h.impl.Requests(), 1)
	implCtx := texts(h.impl.Requests()[0])
	assert.Contains(t, implCtx, "<plan>\nthe plan\n</plan>")
	assert.Contains(t, implCtx, "keep going")
	assert.Less(t, strings.Index(implCtx, "<plan>"), strings.Index(implCtx, "keep going"))

	// the repaired round still reviews and completes
	require.Len(t, h.planner.Requests(), 2)
	assert.Contains(t, texts(h.planner.Requests()[1]), "added the flag")
	assert.False(t, h.ctl.Active())
}

// TestPlanResumeRepairsMissingGoal covers the planning half of the same window:
// a goal recorded as captured whose message never landed must not swallow the
// planning contract from the prompt that retypes it.
func TestPlanResumeRepairsMissingGoal(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: textTurn("a draft")},
		},
		nil,
	)
	require.Empty(t, h.ctl.Start(t.Context(), ""))

	h.crashAt(t, "add a --version flag") // goal captured, the goal turn never ran

	h.resumeFromTranscript(t)

	h.prompt(t, "add a --version flag")
	require.Len(t, h.planner.Requests(), 1)
	planningCtx := texts(h.planner.Requests()[0])
	assert.Contains(t, planningCtx, "planning, not implementing")
	assert.Contains(t, planningCtx, "add a --version flag")
}

// TestPlanResumeLandedKickoffStaysSingle drives a mid-workflow resume whose
// kickoff already landed: the restore must stay silent and the next prompt must
// not carry a second copy of the review kickoff.
func TestPlanResumeLandedKickoffStaysSingle(t *testing.T) {
	t.Parallel()

	// the first review ends with no verdict, so the plain-mode ask fails and the
	// workflow parks in reviewing with both kickoffs delivered
	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"the plan"}`)},
			{Events: textTurn("still reading the diff")},
			{Events: devCallTurn("2", plan.DevCompleteTool, `{}`)},
		},
		[]llm.ScriptedTurn{
			{Events: devCallTurn("3", plan.DevReviewTool, `{"summary":"added the flag"}`)},
		},
	)
	require.Empty(t, h.ctl.Start(t.Context(), ""))
	h.prompt(t, "add a --version flag")
	h.prompt(t, "the plan") // implementing and review kickoffs both land
	require.True(t, h.ctl.Active())

	h.resumeFromTranscript(t)
	require.True(t, h.ctl.Active())
	assert.NotContains(t, strings.Join(h.notices, "\n"), "never reached the transcript")

	_, changed := h.ctl.BeforePrompt(t.Context(), agent.Input{Text: "ship it"})
	assert.False(t, changed) // no repaired kickoff rides ahead of the prompt

	h.prompt(t, "ship it")
	require.False(t, h.ctl.Active())
	finalCtx := texts(h.planner.Requests()[2])
	assert.Equal(t, 1, strings.Count(finalCtx, "<plan>\n"))
	assert.Equal(t, 1, strings.Count(finalCtx, "<implementation_summary>"))
	assert.Contains(t, finalCtx, "ship it")
}

// TestPlanResumeLandedGoalStaysCaptured covers the planning half of the same
// resume: a goal that already landed must stay captured, so a later prompt is
// an ordinary steer rather than the goal all over again.
func TestPlanResumeLandedGoalStaysCaptured(t *testing.T) {
	t.Parallel()

	h := newPlanHarness(t,
		[]llm.ScriptedTurn{
			{Events: textTurn("exploring the tree")},
			{Events: devCallTurn("1", plan.DevImplementTool, `{"plan":"the plan"}`)},
		},
		nil,
	)
	require.Empty(t, h.ctl.Start(t.Context(), ""))
	h.prompt(t, "add a --version flag") // the goal lands, the planner explores
	require.True(t, h.ctl.Active())

	h.resumeFromTranscript(t)
	require.True(t, h.ctl.Active())
	notices := strings.Join(h.notices, "\n")
	assert.NotContains(t, notices, "never reached the transcript")
	assert.NotContains(t, notices, "becomes the goal")

	_, changed := h.ctl.BeforePrompt(t.Context(), agent.Input{Text: "also handle windows"})
	assert.False(t, changed) // an ordinary steer, not the goal retyped

	h.prompt(t, "also handle windows")
	require.Equal(t, []string{"the plan"}, h.editor) // the gate is reached normally
	steerCtx := texts(h.planner.Requests()[1])
	assert.Equal(t, 1, strings.Count(steerCtx, "planning, not implementing"))
	assert.Equal(t, 1, strings.Count(steerCtx, "add a --version flag"))
	assert.Contains(t, steerCtx, "also handle windows")
}
