package agent

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSinkFanout(t *testing.T) {
	t.Parallel()

	// two sinks each receive the full event sequence, in registration order.
	t.Run("forwards_to_every_member", func(t *testing.T) {
		a := newTestAgent(nil, &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolCallEvents("c1", "bash")},
			{Events: textOnly("done")},
		}}, nil)
		set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}
		a.opts.Tools = set

		var one, two recordingSink
		a.sink = &fanoutSink{sinks: []Sink{&one, &two}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "run"}))
		assert.Equal(t, one.calls, two.calls)
		for _, c := range one.calls {
			assert.NotEmpty(t, c) // every method reached each member
		}
	})

	// the ToolStart done closure reaches every member even when a member returns its own per-call closure.
	t.Run("tool_done_calls_every_closure", func(t *testing.T) {
		var one, two recordingSink
		fan := &fanoutSink{sinks: []Sink{&one, &two}}
		done := fan.ToolStart(ToolCall{Name: "bash"}, "")
		assert.NotNil(t, done)
		done(ToolResult{})

		if assert.Len(t, one.calls, 1) {
			assert.Equal(t, "tool_start:bash", one.calls[0])
		}
		if assert.Len(t, two.calls, 1) {
			assert.Equal(t, "tool_start:bash", two.calls[0])
		}
	})
}

// TestMessageObserversFireInOrder asserts every OnMessage fires per appended
// message in registration order.
func TestMessageObserversFireInOrder(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)

	var order [][]string
	record := func(name string) func(MessageInfo) {
		return func(info MessageInfo) {
			if len(order) == 0 || len(order[len(order)-1]) >= 2 {
				order = append(order, []string{name})
				return
			}
			order[len(order)-1] = append(order[len(order)-1], name)
		}
	}
	a.opts.OnMessage = []func(MessageInfo){record("first"), record("second")}

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))

	// the prompt echo and assistant reply each invoke both observers in order.
	assert.Equal(t, [][]string{{"first", "second"}, {"first", "second"}}, order)
}

func TestOnSettled(t *testing.T) {
	t.Parallel()

	// the settled observer runs once after the queues empty on a successful turn.
	t.Run("fires_when_drained", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
		a := newTestAgent(nil, p, nil)

		var settled int
		a.opts.OnSettled = []func(context.Context){func(ctx context.Context) {
			assert.NotNil(t, ctx)
			settled++
		}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Equal(t, 1, settled)
	})

	// an observer that queues another input keeps the same Prompt call alive until everything drains.
	t.Run("observer_can_queue_work", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textOnly("one")},
			{Events: textOnly("two")},
		}}
		a := newTestAgent(nil, p, nil)

		var settled int
		var queued bool
		var nested error
		a.opts.OnSettled = []func(context.Context){func(ctx context.Context) {
			settled++
			if !queued {
				queued = true // queue exactly one follow-up so the run terminates
				nested = a.Prompt(ctx, Input{Text: "next"})
			}
		}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "first"}))
		assert.True(t, queued)
		require.NoError(t, nested)
		assert.Equal(t, 2, settled) // once after each turn drains
	})

	// an observer that queues via Steer keeps the same Prompt call alive until everything drains.
	t.Run("observer_can_steer", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textOnly("one")},
			{Events: textOnly("two")},
		}}
		a := newTestAgent(nil, p, nil)

		var settled int
		var queued bool
		a.opts.OnSettled = []func(context.Context){func(ctx context.Context) {
			settled++
			if !queued {
				queued = true // queue exactly one follow-up via Steer so the run terminates
				require.True(t, a.Steer(Input{Text: "next"}))
			}
		}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "first"}))
		assert.True(t, queued)
		assert.Equal(t, 2, settled) // once after each turn drains
	})

	// an errored turn never reports itself as settled.
	t.Run("not_called_on_errored_turn", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Err: llm.ErrContextOverflow}}}
		a := newTestAgent(nil, p, nil)

		var settled int
		a.opts.OnSettled = []func(context.Context){func(context.Context) { settled++ }}

		err := a.Prompt(t.Context(), Input{Text: "big"})
		require.ErrorIs(t, err, llm.ErrContextOverflow)
		assert.Equal(t, 0, settled)
	})
}
