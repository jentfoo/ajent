package command

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelCommand(t *testing.T) {
	t.Parallel()

	// a named model resolves
	t.Run("resolves_by_name", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("model")
		require.NoError(t, cmd.Handler(t.Context(), "beta", c))
		assert.Equal(t, "beta", c.setModel.ID)
	})

	// an unknown name notifies
	t.Run("unknown_name_notifies", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("model")
		require.NoError(t, cmd.Handler(t.Context(), "nope", c))
		assert.Equal(t, llm.Model{}, c.setModel)
		assert.True(t, c.noticeContains("no model matches nope"))
	})

	// the picker pre-selects the active offset
	t.Run("picker_selects_active_offset", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		// pick index 1; the default active (alpha at index 0) pre-selects that row
		c.picks = []fakePick{{result: 1}}
		cmd, _ := r.Get("model")
		require.NoError(t, cmd.Handler(t.Context(), "", c))
		assert.Equal(t, "beta", c.setModel.ID)
	})

	// a real change persists to the user layer so the next start keeps it
	t.Run("change_persists_user_layer", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("model")
		require.NoError(t, cmd.Handler(t.Context(), "beta", c))
		require.Len(t, c.saveCalls, 1)
		assert.Equal(t, "user", c.saveCalls[0].layer)
		assert.Equal(t, "model", c.saveCalls[0].key)
	})

	// re-selecting the already-active model writes nothing
	t.Run("same_model_writes_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("model")
		require.NoError(t, cmd.Handler(t.Context(), "test/alpha", c))
		assert.Empty(t, c.saveCalls)
	})
}

func TestReasoningCommand(t *testing.T) {
	t.Parallel()

	// a single-option model (only off) reports instead of opening the picker
	t.Run("single_option_reports_not_picker", func(t *testing.T) {
		c := newFakeConsole(t)
		reg, _ := llm.NewRegistry(llm.File{Providers: map[string]llm.ProviderConfig{
			"test": {APIKeyEnv: "TEST_API_KEY", Models: []llm.ModelConfig{{ID: "alpha"}}},
		}}, nil, llm.RegistryOptions{Env: func(string) string { return "" }})
		c.models = reg
		c.state.Model = reg.Active()
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("reasoning")
		require.NoError(t, cmd.Handler(t.Context(), "", c))
		assert.True(t, c.noticeContains("only off available for this model"))
	})

	// a named level is set
	t.Run("sets_level", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("reasoning")
		require.NoError(t, cmd.Handler(t.Context(), "high", c))
		assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
	})

	// an unknown level notifies
	t.Run("unknown_level_notifies", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("reasoning")
		require.NoError(t, cmd.Handler(t.Context(), "bogus", c))
		assert.Equal(t, llm.LevelMedium, c.state.Reasoning.Level) // unchanged
		assert.True(t, c.noticeContains("unknown reasoning level"))
	})

	// a user-off show stays off across a level change
	t.Run("preserves_show_off", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		c.state.Reasoning.Show = false // the /settings off choice
		cmd, _ := r.Get("reasoning")
		require.NoError(t, cmd.Handler(t.Context(), "high", c))
		assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
		assert.False(t, c.state.Reasoning.Show)
	})

	// a user-on show survives a level change too
	t.Run("preserves_show_on", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		cmd, _ := r.Get("reasoning")
		require.NoError(t, cmd.Handler(t.Context(), "high", c))
		assert.True(t, c.state.Reasoning.Show)
	})
}
