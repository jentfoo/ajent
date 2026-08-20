package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlToolExecute(t *testing.T) {
	t.Parallel()

	t.Run("records_and_ends_turn", func(t *testing.T) {
		c, _ := started(t)
		res := call(t, c, DevImplementTool, `{"plan":"do the thing"}`)
		assert.False(t, res.IsError)
		assert.True(t, res.EndTurn)
		require.NotNil(t, c.pending)
		assert.Equal(t, "do the thing", c.pending.payload)
	})

	// the v1 regression: a rejected call must leave the planner free to retry.
	t.Run("empty_payload_keeps_turn", func(t *testing.T) {
		c, _ := started(t)
		res := call(t, c, DevImplementTool, `{"plan":"  "}`)
		assert.True(t, res.IsError)
		assert.False(t, res.EndTurn)
		assert.Nil(t, c.pending)
	})

	t.Run("bad_args_keeps_turn", func(t *testing.T) {
		c, _ := started(t)
		res := call(t, c, DevImplementTool, `not json`)
		assert.True(t, res.IsError)
		assert.False(t, res.EndTurn)
		assert.Nil(t, c.pending)
	})

	t.Run("wrong_phase_keeps_turn", func(t *testing.T) {
		c, _ := started(t)
		res := call(t, c, DevCompleteTool, `{}`)
		assert.True(t, res.IsError)
		assert.False(t, res.EndTurn)
		assert.Contains(t, res.Display, "reviewing")
	})

	t.Run("second_call_rejected", func(t *testing.T) {
		c, _ := started(t)
		first := call(t, c, DevImplementTool, `{"plan":"one"}`)
		require.True(t, first.EndTurn)
		second := call(t, c, DevImplementTool, `{"plan":"two"}`)
		assert.True(t, second.IsError)
		assert.False(t, second.EndTurn)
		assert.Equal(t, "one", c.pending.payload) // the first transition stands
	})

	t.Run("idle_workflow_rejected", func(t *testing.T) {
		c := New(newFakeHost().host())
		res := call(t, c, DevImplementTool, `{"plan":"x"}`)
		assert.True(t, res.IsError)
		assert.False(t, res.EndTurn)
	})

	t.Run("review_summary_required", func(t *testing.T) {
		c, _ := started(t)
		c.phase = PhaseImplementing
		res := call(t, c, DevReviewTool, `{}`)
		assert.True(t, res.IsError) // the reviewer has nothing else to go on
		assert.False(t, res.EndTurn)
		assert.Nil(t, c.pending)

		res = call(t, c, DevReviewTool, `{"summary":"added the flag"}`)
		assert.False(t, res.IsError)
		assert.True(t, res.EndTurn)
		assert.Equal(t, PhaseReviewing, c.pending.to)
	})
}

func TestControlToolsSchemas(t *testing.T) {
	t.Parallel()

	c := New(newFakeHost().host())
	names := make([]string, 0, 4)
	for _, tool := range controlTools(c) {
		names = append(names, tool.Name())
		assert.NotEmpty(t, tool.Description())
		assert.NotEmpty(t, tool.Schema().Parameters)
		assert.NotEmpty(t, tool.Label(agentCall(tool.Name())))
	}
	assert.Equal(t,
		[]string{DevImplementTool, DevReviewTool, DevReviseTool, DevCompleteTool}, names)
}
