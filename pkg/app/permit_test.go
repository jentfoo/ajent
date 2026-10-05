package app

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
)

// countReserve records every reservation the batch hook makes.
type countReserve struct{ n atomic.Int32 }

func (r *countReserve) Reserve([]agent.ToolCall) { r.n.Add(1) }

func TestWireBatchHooks(t *testing.T) {
	t.Parallel()

	barrier := permit.NewBarrier(func(string) bool { return false }, permit.Options{})

	t.Run("delegates_to_reserve", func(t *testing.T) {
		var opts agent.Options
		res := &countReserve{}
		wireBatchHooks(&opts, res, barrier)
		require.NotNil(t, opts.OnToolBatch)

		opts.OnToolBatch(t.Context(), nil)
		assert.Positive(t, res.n.Load())
	})

	// Driver and RunHeadless must wire before agent.New copies Options: a hook
	// attached to the local afterwards never reaches the running agent.
	t.Run("hook_reaches_a_live_turn", func(t *testing.T) {
		var opts agent.Options
		res := &countReserve{}
		wireBatchHooks(&opts, res, barrier)

		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: mainToolTurn()},
			{Events: textTurnRewind("done")},
		}}
		a := agent.New(&agent.State{Model: llm.Model{ID: "test"}}, agent.Options{
			Provider:    func(llm.Model) (llm.Provider, error) { return p, nil },
			Sinks:       []agent.Sink{agent.NopSink{}},
			Tools:       singleToolSet{tool: noopRewindTool{}},
			Env:         agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
			OnToolBatch: opts.OnToolBatch,
		})
		require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "hi"}))
		assert.Positive(t, res.n.Load())
	})
}
