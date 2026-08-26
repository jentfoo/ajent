package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverflowRetry(t *testing.T) {
	t.Parallel()

	// a first request too big is compacted then retried successfully.
	t.Run("retries_after_compact", func(t *testing.T) {
		var reasons []CompactReason
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Err: llm.ErrContextOverflow},   // first request is too big
			{Events: textOnly("recovered")}, // the compacted retry succeeds
		}}
		a := New(&State{Model: llm.Model{ID: "test"}}, Options{
			Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
			Env:      testEnv,
			Compact: func(_ context.Context, r CompactReason) (bool, error) {
				reasons = append(reasons, r)
				return true, nil // report that compaction changed the context
			},
		})

		err := a.Prompt(t.Context(), Input{Text: "big"})
		require.NoError(t, err)
		// the overflow retry fires mid-turn, then the threshold hook fires at the
		// turn boundary once the retried turn completes.
		assert.Equal(t, []CompactReason{CompactOverflow, CompactThreshold}, reasons)

		// the retried response landed as the assistant reply
		var sb strings.Builder
		for _, m := range a.state.Messages {
			if m.Role != llm.RoleAssistant {
				continue
			}
			for _, b := range m.Content {
				if tb, ok := b.(llm.TextBlock); ok {
					sb.WriteString(tb.Text)
				}
			}
		}
		assert.Equal(t, "recovered", sb.String())
	})

	// still overflowing after one compaction fails the turn.
	t.Run("retries_at_most_once", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Err: llm.ErrContextOverflow},
			{Err: llm.ErrContextOverflow}, // still overflowing after one compaction
		}}
		var calls int
		a := New(&State{Model: llm.Model{ID: "test"}}, Options{
			Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
			Env:      testEnv,
			Compact: func(_ context.Context, _ CompactReason) (bool, error) {
				calls++
				return true, nil
			},
		})

		err := a.Prompt(t.Context(), Input{Text: "big"})
		require.ErrorIs(t, err, llm.ErrContextOverflow)
		assert.Equal(t, 1, calls) // one compaction attempt per turn, then it fails
	})

	// with no compact hook wired the overflow fails the turn outright.
	t.Run("no_hook_fails_turn", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Err: llm.ErrContextOverflow}}}
		a := New(&State{Model: llm.Model{ID: "test"}}, Options{
			Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
			Env:      testEnv,
		})
		err := a.Prompt(t.Context(), Input{Text: "big"})
		require.ErrorIs(t, err, llm.ErrContextOverflow)
	})
}

func TestThresholdHookAtTurnBoundary(t *testing.T) {
	t.Parallel()

	var reasons []CompactReason
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := New(&State{Model: llm.Model{ID: "test"}}, Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Env:      testEnv,
		Compact: func(_ context.Context, r CompactReason) (bool, error) {
			reasons = append(reasons, r)
			return false, nil // below threshold, nothing to do
		},
	})

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
	assert.Equal(t, []CompactReason{CompactThreshold}, reasons)
}

func TestWithStateMutatesLiveState(t *testing.T) {
	t.Parallel()

	a := New(&State{Model: llm.Model{ID: "test"}}, Options{
		Provider: func(llm.Model) (llm.Provider, error) { return nil, nil },
	})
	ok := a.WithState(func(s *State) { s.Messages = []llm.Message{llm.Text(llm.RoleUser, "x")} })
	assert.True(t, ok)
	require.Len(t, a.state.Messages, 1)
}

func TestWithStateRefusedWhileRunning(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := New(&State{Model: llm.Model{ID: "test"}}, Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Env:      testEnv,
		Tools:    set,
	})

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	assert.False(t, a.WithState(func(*State) {})) // refused mid-turn
	close(block)
	require.NoError(t, <-errCh)

	assert.True(t, a.WithState(func(*State) {})) // accepted once at rest
}
