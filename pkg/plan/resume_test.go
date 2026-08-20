package plan

import (
	"testing"

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

	t.Run("active_workflow_is_kept", func(t *testing.T) {
		c, f := started(t)
		f.mu.Lock()
		f.stored = mid()
		f.mu.Unlock()
		assert.False(t, c.Restore())
		assert.Equal(t, PhasePlanning, c.phase)
	})
}

func TestControllerPersistLocked(t *testing.T) {
	t.Parallel()

	c, f := started(t)
	handOff(t, c, "the plan")
	submitPlan(t, c, "edited plan")

	require.NotEmpty(t, f.persisted)
	last := f.persisted[len(f.persisted)-1]
	assert.Equal(t, PhaseImplementing, last.Phase)
	assert.Equal(t, "edited plan", last.ApprovedPlan) // the approved text, not the draft
	assert.Equal(t, plannerModel.Key(), last.Planner)
	assert.Equal(t, []string{"read", "write", "edit", "bash"}, last.SavedTools)
}
