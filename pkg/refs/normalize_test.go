package refs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

func TestNormalize(t *testing.T) {
	t.Parallel()

	t.Run("nil_expander_passthrough", func(t *testing.T) {
		t.Parallel()

		in := agent.Input{Text: "see @a.go"}
		assert.Equal(t, in, Normalize(nil, in, func(string) {}))
	})

	t.Run("expands_like_fresh_prompt", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644))
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		x := NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir})

		in := Normalize(x, agent.Input{Text: "look at @a.go"}, func(string) {})
		assert.Contains(t, in.Text, "@a.go")
		require.NotNil(t, in.After)
		assert.True(t, in.Prepared)

		msgs := in.After(t.Context())
		require.Len(t, msgs, 2) // the read pair lands behind the message
	})

	t.Run("notices_surface", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		x := NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir})

		var warned []string
		_ = Normalize(x, agent.Input{Text: "see @missing.go"}, func(n string) { warned = append(warned, n) })
		require.Len(t, warned, 1)
		assert.Contains(t, warned[0], "missing.go")
	})

	t.Run("assembled_context_skips_expansion", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		x := NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir})

		in := agent.Input{
			Text:   "look at @a.go",
			Before: []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "staged")}},
		}
		got := Normalize(x, in, func(string) { t.Error("unexpected notice") })
		assert.Equal(t, in, got)

		in = agent.Input{Blocks: llm.BlockList{llm.TextBlock{Text: "look at @a.go"}}}
		got = Normalize(x, in, func(string) { t.Error("unexpected notice") })
		assert.Equal(t, in, got)

		in = agent.Input{Text: "look at @a.go", After: func(context.Context) []llm.Message { return nil }}
		got = Normalize(x, in, func(string) { t.Error("unexpected notice") })
		assert.Equal(t, in.Text, got.Text)
	})
}
