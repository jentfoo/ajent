package app

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// countReserve records every reservation the batch hook makes.
type countReserve struct{ n atomic.Int32 }

// emptyToolSource satisfies subagent.ToolSource over no tools, standing in for
// the parent registry view production wiring always supplies.
type emptyToolSource struct{}

func (emptyToolSource) All() []agent.Tool    { return nil }
func (emptyToolSource) ReadOnly(string) bool { return false }

func (r *countReserve) Reserve([]agent.ToolCall) { r.n.Add(1) }

// wiredBarrier builds a production-shaped barrier over reg: the inputs both
// front ends install unconditionally, with no-op sinks and an unsure classifier
// standing in for tests that never reach a classification.
func wiredBarrier(reg *tools.Registry) *permit.Barrier {
	return permit.NewBarrier(reg.ReadOnly, permit.Options{
		Classifier: permit.NewCachedClassifier(
			func(context.Context, permit.Subject) permit.Class { return permit.ClassUnsure }),
		Notice: func(string) {},
		DryRun: reg.DryRun,
	})
}

func TestWireBatchHooks(t *testing.T) {
	t.Parallel()

	barrier := wiredBarrier(tools.New())

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
		a := agent.New(&agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}, agent.Options{
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
