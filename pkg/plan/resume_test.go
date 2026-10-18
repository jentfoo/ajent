package plan

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerRestore(t *testing.T) {
	t.Parallel()

	mid := func() *persisted {
		return &persisted{
			Phase:          PhaseReviewing,
			Planner:        plannerModel.Key(),
			Implementor:    implementorModel.Key(),
			SavedModel:     implementorModel.Key(),
			SavedTools:     []string{"read", "bash"},
			ApprovedPlan:   "the plan",
			RevisionRounds: []string{"fix it"},
			GoalCaptured:   true,
			PlanTip:        "plan-tip",
			ReviewTip:      "review-tip",
		}
	}

	t.Run("mid_workflow_rebuilds", func(t *testing.T) {
		f := newFakeHost()
		f.stored = mid()
		c := New(f.host())

		require.True(t, c.Restore())
		assert.Equal(t, PhaseReviewing, c.phase)
		assert.Equal(t, plannerModel, c.planner)
		assert.Equal(t, implementorModel, c.implementor)
		assert.Equal(t, "the plan", c.approvedPlan)
		assert.Equal(t, 2, c.round())
		assert.Equal(t, "plan-tip", c.planTip)
		assert.Len(t, f.added, 4)                        // control tools re-registered
		assert.Contains(t, f.lastTools(), DevReviseTool) // review scope re-applied
	})

	t.Run("restored_scope_restores_saved_tools", func(t *testing.T) {
		f := newFakeHost()
		f.stored = mid()
		c := New(f.host())
		require.True(t, c.Restore())

		c.Stop()
		assert.Equal(t, []string{"read", "bash"}, f.lastTools())
	})

	t.Run("terminal_never_resurrects", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Phase = PhaseDone
		f.stored = p
		c := New(f.host())

		assert.False(t, c.Restore())
		assert.False(t, c.Active())
	})

	t.Run("nothing_stored", func(t *testing.T) {
		f := newFakeHost()
		c := New(f.host())
		assert.False(t, c.Restore())
	})

	t.Run("unresolvable_model_abandons", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Planner = "p/gone"
		f.stored = p
		c := New(f.host())

		assert.False(t, c.Restore())
		assert.False(t, c.Active())
		assert.Contains(t, f.notices[len(f.notices)-1], "is unavailable")
	})

	t.Run("unresolvable_saved_model_abandons", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.SavedModel = "p/gone"
		f.stored = p
		c := New(f.host())

		assert.False(t, c.Restore())
		assert.False(t, c.Active()) // not silently rebuilt around the implementor
		assert.Contains(t, f.notices[len(f.notices)-1], "saved model")
	})

	t.Run("missing_saved_model_falls_back", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.SavedModel = "" // an entry predating the saved-model field
		f.stored = p
		c := New(f.host())

		require.True(t, c.Restore())
		assert.Equal(t, implementorModel, c.savedModel)
	})

	t.Run("active_workflow_is_kept", func(t *testing.T) {
		c, f := started(t)
		f.mu.Lock()
		f.stored = mid()
		f.mu.Unlock()
		assert.False(t, c.Restore())
		assert.Equal(t, PhasePlanning, c.phase)
	})

	t.Run("awaiting_gate_refills_draft", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Phase = PhaseAwaitingPlan
		p.DraftPlan = "the draft"
		p.ApprovedPlan = "" // nothing approved while parked at the gate
		f.stored = p
		c := New(f.host())

		require.True(t, c.Restore())
		assert.Equal(t, PhaseAwaitingPlan, c.phase)
		assert.Contains(t, f.inputs, "the draft") // the editor is refilled for editing
	})

	// a crash between persisting a transition and delivering its message leaves
	// the phase recorded with nothing behind it
	t.Run("missing_kickoff_is_repaired", func(t *testing.T) {
		for _, phase := range []Phase{PhaseImplementing, PhaseReviewing} {
			f := newFakeHost()
			p := mid()
			p.Phase = phase
			f.stored = p
			f.landed = false
			c := New(f.host())

			require.True(t, c.Restore())
			in, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "keep going"})
			require.True(t, ok)
			require.Len(t, in.Before, 1) // the kickoff rides ahead of the prompt
			assert.Contains(t, in.Before[0].Message.Content[0].(llm.TextBlock).Text,
				"<plan>\nthe plan\n</plan>")
			assert.Contains(t, f.notices[len(f.notices)-2], "never reached the transcript")
		}
	})

	// a goal recorded as captured whose message never landed must not swallow
	// the planning contract from the prompt that retypes it
	t.Run("missing_goal_is_recaptured", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Phase = PhasePlanning
		p.GoalCaptured = true
		f.stored = p
		f.landed = false
		c := New(f.host())

		require.True(t, c.Restore())
		in, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "add a flag"})
		require.True(t, ok) // the retype becomes the goal and gains the contract
		require.Len(t, in.Blocks, 1)
		assert.Contains(t, in.Blocks[0].(llm.TextBlock).Text, "planning, not implementing")
		assert.Contains(t, f.notices[len(f.notices)-2], "becomes the goal")
	})

	t.Run("landed_transition_needs_no_repair", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Phase = PhaseImplementing
		f.stored = p // landed defaults true
		c := New(f.host())

		require.True(t, c.Restore())
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "keep going"})
		assert.False(t, ok) // passes through, the round has its kickoff
	})

	// a parked gate is legitimately entry-tailed: the draft refill is all it needs
	t.Run("missing_landing_at_gate_ignored", func(t *testing.T) {
		f := newFakeHost()
		p := mid()
		p.Phase = PhaseAwaitingPlan
		p.DraftPlan = "the draft"
		f.stored = p
		f.landed = false
		c := New(f.host())

		require.True(t, c.Restore())
		assert.Equal(t, PhaseAwaitingPlan, c.phase)
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "the draft"})
		require.True(t, ok) // the gate itself, not a kickoff repair
	})
}

func TestControllerPersistLocked(t *testing.T) {
	t.Parallel()

	c, f := started(t)
	handOff(t, c, "the plan")
	submitPlan(t, c, "edited plan")

	require.NotEmpty(t, f.persisted)
	// the gate entry carries the draft so a resume restores it
	gated := f.persisted[len(f.persisted)-2]
	assert.Equal(t, PhaseAwaitingPlan, gated.Phase)
	assert.Empty(t, gated.ApprovedPlan) // nothing approved yet at the gate
	assert.Equal(t, "the plan", gated.DraftPlan)
	last := f.persisted[len(f.persisted)-1]
	assert.Equal(t, PhaseImplementing, last.Phase)
	assert.Equal(t, "edited plan", last.ApprovedPlan) // the approved text, not the draft
	assert.Empty(t, last.DraftPlan)                   // gate passed, no draft lingers
	assert.Equal(t, plannerModel.Key(), last.Planner)
	assert.Equal(t, []string{"read", "write", "edit", "bash"}, last.SavedTools)
}
