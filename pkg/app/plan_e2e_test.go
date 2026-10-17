package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	dir      string
	planner  *llm.ScriptedProvider
	impl     *llm.ScriptedProvider
	editor   []string
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
		st: st, rec: rec, toolsReg: toolsReg, dir: dir,
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

	h.ctl = plan.New(plan.Host{
		PickModel:   func(context.Context, string) (llm.Model, bool) { return e2ePlanner, true },
		ActiveModel: func() llm.Model { return st.Model },
		Running:     h.ag.Running,
		Abort:       h.ag.Interrupt,

		ToolNames:    toolsReg.Names,
		PlannerTools: func() []string { return plannerExtras(toolsReg) },
		SetTools:     toolsReg.SetEnabled,
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
			entries, _, rerr := session.Read(w.Path())
			if rerr != nil {
				return false
			}
			return session.LatestCustom(session.Branch(entries, w.Head()), plan.CustomType, v)
		},
		ResolveModel: func(key string) (llm.Model, bool) {
			switch key {
			case e2ePlanner.Key():
				return e2ePlanner, true
			case e2eImplementor.Key():
				return e2eImplementor, true
			}
			return llm.Model{}, false
		},

		LastText: func() string { return llm.LastAssistantText(st.Messages) },
		SetInput: func(s string) { h.editor = append(h.editor, s) },
		Git:      func(context.Context) (string, string) { return " M main.go", " main.go | 2 +-" },
	})
	return h
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
