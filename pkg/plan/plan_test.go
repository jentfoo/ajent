package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerStart(t *testing.T) {
	t.Parallel()

	t.Run("applies_planning_scope", func(t *testing.T) {
		c, f := started(t)
		assert.Equal(t, PhasePlanning, c.phase)
		assert.Equal(t, plannerModel, c.planner)
		assert.Equal(t, implementorModel, c.implementor)
		assert.Equal(t, []string{"read", "grep", "find", "ls", "bash", "agent_start",
			AskUserTool, DevImplementTool}, f.lastTools())
		assert.Len(t, f.added, 4)
		// planning stays on the current branch, but must run on the planner model
		require.Len(t, f.forks, 1)
		assert.Equal(t, "trunk-tip", f.forks[0].head)
		assert.Equal(t, plannerModel, f.forks[0].model)
	})

	t.Run("prefills_editor", func(t *testing.T) {
		f := newFakeHost()
		c := New(f.host())
		require.Empty(t, c.Start(t.Context(), "add a flag"))
		assert.Equal(t, []string{"add a flag"}, f.inputs)
	})

	t.Run("cancelled_pick_does_nothing", func(t *testing.T) {
		f := newFakeHost()
		f.pickOK = false
		c := New(f.host())
		assert.Equal(t, "cancelled", c.Start(t.Context(), ""))
		assert.False(t, c.Active())
		assert.Empty(t, f.toolSets)
	})

	t.Run("refuses_while_running", func(t *testing.T) {
		f := newFakeHost()
		f.running = true
		c := New(f.host())
		assert.Contains(t, c.Start(t.Context(), ""), "turn is running")
	})

	t.Run("refuses_when_active", func(t *testing.T) {
		c, _ := started(t)
		assert.Contains(t, c.Start(t.Context(), ""), "already running")
	})
}

func TestControllerStop(t *testing.T) {
	t.Parallel()

	t.Run("idle_restores_scope", func(t *testing.T) {
		c, f := started(t)
		c.Stop()
		assert.Equal(t, PhaseDone, c.phase)
		assert.Equal(t, []string{"read", "write", "edit", "bash"}, f.lastTools())
		assert.Equal(t, 1, f.dropped)
		assert.Empty(t, c.Status())
	})

	t.Run("running_aborts_first", func(t *testing.T) {
		c, f := started(t)
		f.mu.Lock()
		f.running = true
		f.mu.Unlock()

		c.Stop()
		assert.Equal(t, 1, f.aborted)
		assert.Equal(t, PhasePlanning, c.phase) // the turn boundary completes it

		f.mu.Lock()
		f.running = false
		f.mu.Unlock()
		_, ok := c.Advance(t.Context(), done())
		assert.False(t, ok)
		assert.Equal(t, PhaseDone, c.phase)
	})

	t.Run("idle_controller_notifies", func(t *testing.T) {
		f := newFakeHost()
		c := New(f.host())
		c.Stop()
		assert.Contains(t, f.notices, "no plan workflow is running")
	})

	t.Run("returns_to_review_tip", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)

		c.Stop()
		last := f.forks[len(f.forks)-1]
		assert.Equal(t, c.planTip, last.head) // the live review branch
		assert.Equal(t, implementorModel, last.model)
	})
}

func TestControllerFocus(t *testing.T) {
	t.Parallel()

	c, _ := started(t)
	assert.Empty(t, c.Focus()) // planning uses the unguided summary

	handOff(t, c, "the plan")
	submitPlan(t, c, "the plan")
	assert.Equal(t, implementFocus, c.Focus())

	_, ok := c.Advance(t.Context(), done())
	require.True(t, ok)
	assert.Equal(t, reviewFocus, c.Focus())

	c.Stop()
	assert.Empty(t, c.Focus())
}

func TestControllerToolsFor(t *testing.T) {
	t.Parallel()

	c, _ := started(t)
	impl := c.toolsFor(PhaseImplementing)
	assert.Equal(t, []string{"read", "write", "edit", "bash", DevReviewTool}, impl)

	c.savedTools = nil // a workflow started without a readable tool set
	assert.Equal(t, []string{"read", "write", "edit", "bash", DevReviewTool},
		c.toolsFor(PhaseImplementing))
	c.savedTools = []string{"read", "write", "edit", "bash"}

	review := c.toolsFor(PhaseReviewing)
	assert.Contains(t, review, DevReviseTool)
	assert.Contains(t, review, DevCompleteTool)
	assert.NotContains(t, review, "write") // read-only by construction
	assert.NotContains(t, review, "edit")
	assert.NotContains(t, review, DevImplementTool)
}

func TestControllerStatus(t *testing.T) {
	t.Parallel()

	f := newFakeHost()
	c := New(f.host())
	assert.Empty(t, c.Status())

	require.Empty(t, c.Start(t.Context(), ""))
	assert.Contains(t, c.Status(), "phase=planning")
	assert.Contains(t, c.Status(), "round=1/4")
	assert.Contains(t, f.statuses, "plan: planning (r1/4)")
}
