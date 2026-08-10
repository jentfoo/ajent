package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScriptedProviderStream(t *testing.T) {
	t.Parallel()

	t.Run("replays_turns_in_order", func(t *testing.T) {
		p := &ScriptedProvider{Turns: []ScriptedTurn{
			{Events: []Event{{Type: EventTextDelta, Text: "first"}}},
			{Events: []Event{{Type: EventTextDelta, Text: "second"}}},
		}}

		for _, want := range []string{"first", "second"} {
			s, err := p.Stream(t.Context(), Request{})
			require.NoError(t, err)
			ev, ok := s.Next()
			require.True(t, ok)
			assert.Equal(t, want, ev.Text)
			require.NoError(t, s.Close())
		}
	})
	t.Run("records_requests", func(t *testing.T) {
		p := &ScriptedProvider{Turns: []ScriptedTurn{{}, {}}}
		_, err := p.Stream(t.Context(), Request{MaxTokens: 10})
		require.NoError(t, err)
		_, err = p.Stream(t.Context(), Request{MaxTokens: 20})
		require.NoError(t, err)

		got := p.Requests()
		require.Len(t, got, 2)
		assert.Equal(t, 10, got[0].MaxTokens)
		assert.Equal(t, 20, got[1].MaxTokens)
	})
	t.Run("exhausted_script_errors", func(t *testing.T) {
		p := &ScriptedProvider{}
		_, err := p.Stream(t.Context(), Request{})
		assert.ErrorIs(t, err, ErrScriptExhausted)
	})
	t.Run("turn_error_returned", func(t *testing.T) {
		want := errors.New("rate limited")
		p := &ScriptedProvider{Turns: []ScriptedTurn{{Err: want}}}
		_, err := p.Stream(t.Context(), Request{})
		assert.ErrorIs(t, err, want)
	})
	t.Run("cancelled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		p := &ScriptedProvider{Turns: []ScriptedTurn{{}}}
		_, err := p.Stream(ctx, Request{})
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestScriptedProviderName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "scripted", (&ScriptedProvider{}).Name())
	assert.Equal(t, "lmstudio", (&ScriptedProvider{ProviderName: "lmstudio"}).Name())
}
