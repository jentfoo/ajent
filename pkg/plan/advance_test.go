package plan

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handOff drives planning through dev_implement to the awaiting-plan gate.
func handOff(t *testing.T, c *Controller, plan string) {
	t.Helper()

	require.True(t, call(t, c, DevImplementTool, `{"plan":`+quote(plan)+`}`).EndTurn)
	in, ok := c.Advance(t.Context(), done())
	require.False(t, ok) // the gate starts no turn
	require.Empty(t, in.Text)
}

// submitPlan puts the (possibly edited) plan through the pump seam.
func submitPlan(t *testing.T, c *Controller, text string) agent.Input {
	t.Helper()

	in, ok := c.BeforePrompt(t.Context(), agent.Input{Text: text})
	require.True(t, ok)
	return in
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func TestControllerBeforePrompt(t *testing.T) {
	t.Parallel()

	t.Run("first_goal_gains_contract", func(t *testing.T) {
		c, _ := started(t)
		in, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "add a flag"})
		require.True(t, ok)
		assert.Equal(t, "add a flag", in.Text) // the echoed line is untouched
		require.Len(t, in.Blocks, 1)
		assert.Contains(t, in.Blocks[0].(llm.TextBlock).Text, "planning, not implementing")
	})

	t.Run("later_goals_pass_through", func(t *testing.T) {
		c, _ := started(t)
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "add a flag"})
		require.True(t, ok)
		_, ok = c.BeforePrompt(t.Context(), agent.Input{Text: "also this"})
		assert.False(t, ok)
	})

	t.Run("injected_input_passes_through", func(t *testing.T) {
		c, _ := started(t)
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "argv prompt", Injected: true})
		assert.False(t, ok)
	})

	t.Run("edited_plan_becomes_kickoff", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "draft plan")
		in := submitPlan(t, c, "edited plan")

		assert.Equal(t, PhaseImplementing, c.phase)
		assert.Contains(t, in.Text, "<plan>\nedited plan\n</plan>")
		assert.NotContains(t, in.Text, "draft plan")
		require.NotEmpty(t, f.forks)
		last := f.forks[len(f.forks)-1]
		assert.Empty(t, last.head) // a brand-new root
		assert.Equal(t, implementorModel, last.model)
	})

	// the pump flushes staged shell results into Before, clipboard images into
	// Blocks and planned @ reads into After; any would pin itself around the
	// kickoff in the fresh root
	t.Run("kickoff_drops_flushed_context", func(t *testing.T) {
		c, _ := started(t)
		handOff(t, c, "draft plan")
		in, ok := c.BeforePrompt(t.Context(), agent.Input{
			Text:   "edited plan",
			Blocks: llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: []byte("img")}},
			Before: []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "User Ran: make test")}},
			After:  func(context.Context) []llm.Message { return []llm.Message{llm.Text(llm.RoleUser, "read pair")} },
		})
		require.True(t, ok)

		assert.Equal(t, PhaseImplementing, c.phase)
		assert.Contains(t, in.Text, "<plan>\nedited plan\n</plan>")
		assert.Empty(t, in.Before) // staged results never reach the new root
		assert.Empty(t, in.Blocks)
		assert.Nil(t, in.After)     // planned reads never land behind the kickoff
		assert.True(t, in.Prepared) // workflow text, the expansion seam skips it
	})

	t.Run("injected_input_never_approves", func(t *testing.T) {
		c, _ := started(t)
		handOff(t, c, "draft plan")
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "staged flush", Injected: true})

		assert.False(t, ok) // the gate is untouched, nothing becomes approved
		assert.Equal(t, PhaseAwaitingPlan, c.phase)
		assert.Empty(t, c.approvedPlan)
	})

	// a paused workflow's first hand-continued turn gets the full budget back,
	// rather than re-pausing on the next error with zero attempts
	t.Run("hand_continue_resets_retries", func(t *testing.T) {
		c, _ := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		failed := agent.TurnResult{Stop: llm.StopError, Err: assert.AnError}
		for i := 0; i < maxExecRetries; i++ {
			in, ok := c.Advance(t.Context(), failed)
			require.True(t, ok) // the retries the budget allows
			assert.Contains(t, in.Text, "Continue the implementation")
		}
		_, ok := c.Advance(t.Context(), failed)
		require.False(t, ok) // budget spent: paused in place

		_, changed := c.BeforePrompt(t.Context(), agent.Input{Text: "keep going"})
		require.False(t, changed) // the input passes through untouched
		for i := 0; i < maxExecRetries; i++ {
			in, ok := c.Advance(t.Context(), failed)
			require.True(t, ok) // a fresh budget retries instead of re-pausing
			assert.Contains(t, in.Text, "Continue the implementation")
		}
	})

	// injected input is not hand continuation, so it leaves the budget alone
	t.Run("injected_submit_keeps_budget", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		failed := agent.TurnResult{Stop: llm.StopError, Err: assert.AnError}
		_, ok := c.Advance(t.Context(), failed)
		require.True(t, ok) // one retry spent

		_, changed := c.BeforePrompt(t.Context(), agent.Input{Text: "staged flush", Injected: true})
		require.False(t, changed)

		_, ok = c.Advance(t.Context(), failed)
		require.True(t, ok) // the second error still retries rather than pausing
		assert.Contains(t, f.notices[len(f.notices)-1], "(2/3)")
	})

	t.Run("idle_controller_passes_through", func(t *testing.T) {
		c := New(newFakeHost().host())
		_, ok := c.BeforePrompt(t.Context(), agent.Input{Text: "hi"})
		assert.False(t, ok)
	})
}

func TestControllerAdvance(t *testing.T) {
	t.Parallel()

	t.Run("plan_fills_editor_only", func(t *testing.T) {
		c, f := started(t)
		before := len(f.forks)
		handOff(t, c, "the plan")
		assert.Equal(t, PhaseAwaitingPlan, c.phase)
		assert.Contains(t, f.inputs, "the plan")
		assert.Len(t, f.forks, before) // nothing branches until the user submits
	})

	t.Run("planner_question_stays", func(t *testing.T) {
		c, _ := started(t)
		_, ok := c.Advance(t.Context(), done())
		assert.False(t, ok)
		assert.Equal(t, PhasePlanning, c.phase) // no transition was recorded
	})

	t.Run("implementor_stop_starts_review", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		in, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		assert.Equal(t, PhaseReviewing, c.phase)
		assert.True(t, in.Prepared) // quoted @ paths stay literal for the reviewer
		assert.Contains(t, in.Text, "<plan>\nthe plan\n</plan>")
		assert.Contains(t, in.Text, "M main.go")
		last := f.forks[len(f.forks)-1]
		assert.Equal(t, c.planTip, last.head) // review round 1 forks the plan tip
		assert.Equal(t, plannerModel, last.model)
	})

	// the implementor stopped without dev_review: the reviewer still has to hear
	// what happened, so its closing message stands in as the summary.
	t.Run("unreported_round_falls_back", func(t *testing.T) {
		c, f := started(t)
		f.mu.Lock()
		f.messages = []llm.Message{llm.Text(llm.RoleAssistant, "wrote the flag and its test")}
		f.mu.Unlock()
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		in, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		assert.Contains(t, in.Text, "<implementation_summary>")
		assert.Contains(t, in.Text, "wrote the flag and its test")
	})

	t.Run("reported_summary_wins", func(t *testing.T) {
		c, f := started(t)
		f.mu.Lock()
		f.messages = []llm.Message{llm.Text(llm.RoleAssistant, "trailing chatter")}
		f.mu.Unlock()
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		require.True(t, call(t, c, DevReviewTool, `{"summary":"added the flag"}`).EndTurn)

		in, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		assert.Contains(t, in.Text, "added the flag")
		assert.NotContains(t, in.Text, "trailing chatter")
	})

	t.Run("complete_restores_scope", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		require.True(t, call(t, c, DevCompleteTool, `{}`).EndTurn)

		_, ok = c.Advance(t.Context(), done())
		assert.False(t, ok)
		assert.Equal(t, PhaseDone, c.phase)
		assert.Equal(t, []string{"read", "write", "edit", "bash"}, f.lastTools())
		assert.Equal(t, 1, f.dropped)
	})

	t.Run("revise_opens_new_root", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		require.True(t, call(t, c, DevReviseTool, `{"instructions":"handle the empty case"}`).EndTurn)

		in, ok := c.Advance(t.Context(), done())
		require.True(t, ok)
		assert.Equal(t, PhaseImplementing, c.phase)
		assert.True(t, in.Prepared) // instructions quote paths as pointers
		assert.Contains(t, in.Text, "handle the empty case")
		assert.Contains(t, in.Text, "<plan>\nthe plan\n</plan>")
		assert.Empty(t, f.forks[len(f.forks)-1].head)

		// the next review continues the review branch rather than the plan tip
		_, ok = c.Advance(t.Context(), done())
		require.True(t, ok)
		assert.Equal(t, c.reviewTip, f.forks[len(f.forks)-1].head)
	})

	t.Run("aborted_turn_stops", func(t *testing.T) {
		c, f := started(t)
		_, ok := c.Advance(t.Context(), agent.TurnResult{Stop: llm.StopAborted})
		assert.False(t, ok)
		assert.Equal(t, PhaseDone, c.phase)
		assert.Equal(t, []string{"read", "write", "edit", "bash"}, f.lastTools())
	})

	t.Run("implementor_error_retries", func(t *testing.T) {
		c, _ := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		failed := agent.TurnResult{Stop: llm.StopError, Err: assert.AnError}
		for i := 1; i <= maxExecRetries; i++ {
			in, ok := c.Advance(t.Context(), failed)
			require.True(t, ok)
			assert.Contains(t, in.Text, "Continue the implementation")
			assert.Equal(t, PhaseImplementing, c.phase)
		}
		_, ok := c.Advance(t.Context(), failed)
		assert.False(t, ok)
		assert.Equal(t, PhaseImplementing, c.phase) // paused in place, not advanced
	})

	// a truncated turn stopped short of the work, so reviewing it would judge an
	// implementation that never finished
	t.Run("truncated_turn_retries", func(t *testing.T) {
		c, _ := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")

		in, ok := c.Advance(t.Context(), agent.TurnResult{Stop: llm.StopMaxTokens})
		require.True(t, ok)
		assert.Contains(t, in.Text, "Continue the implementation")
		assert.True(t, in.Prepared)
		assert.Equal(t, PhaseImplementing, c.phase)
	})

	t.Run("failed_fork_is_reported", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		f.mu.Lock()
		f.forkErr = assert.AnError
		f.mu.Unlock()

		submitPlan(t, c, "the plan")
		assert.Contains(t, f.notices[len(f.notices)-2], "could not complete the branch switch")
	})

	t.Run("revision_cap_reports", func(t *testing.T) {
		c, f := started(t)
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		for i := 0; i < maxRevisions; i++ {
			_, ok := c.Advance(t.Context(), done()) // implementation -> review
			require.True(t, ok)
			require.True(t, call(t, c, DevReviseTool, `{"instructions":"round `+strconv.Itoa(i)+`"}`).EndTurn)
			if _, ok = c.Advance(t.Context(), done()); !ok {
				break
			}
		}
		assert.Equal(t, PhaseDone, c.phase)
		require.NotEmpty(t, f.notices) // the cap is reported, not silently dropped
		assert.Contains(t, f.notices[len(f.notices)-1], "revision limit")
	})

	t.Run("stalled_reviewer_completes", func(t *testing.T) {
		c, f := started(t)
		f.askIndex = 1 // "Accept and finish"
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)

		_, ok = c.Advance(t.Context(), done()) // review ended with no verdict
		assert.False(t, ok)
		assert.Equal(t, PhaseDone, c.phase)
	})

	t.Run("stalled_reviewer_keeps_reviewing", func(t *testing.T) {
		c, f := started(t)
		f.askIndex = 2 // "Keep reviewing"
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)

		_, ok = c.Advance(t.Context(), done()) // review ended with no verdict
		assert.False(t, ok)
		assert.Equal(t, PhaseReviewing, c.phase)
	})

	// a nil Ask is documented valid on Host: without an asker the stall pauses
	// in place for hand steering rather than panicking
	t.Run("stalled_reviewer_nil_ask", func(t *testing.T) {
		f := newFakeHost()
		h := f.host()
		h.Ask = nil
		c := New(h)
		require.Empty(t, c.Start(t.Context(), ""))
		handOff(t, c, "the plan")
		submitPlan(t, c, "the plan")
		_, ok := c.Advance(t.Context(), done())
		require.True(t, ok)

		_, ok = c.Advance(t.Context(), done()) // review ended with no verdict
		assert.False(t, ok)
		assert.Equal(t, PhaseReviewing, c.phase) // paused in place, not stopped
		assert.Contains(t, f.notices[len(f.notices)-1], "without a verdict")
	})
}
