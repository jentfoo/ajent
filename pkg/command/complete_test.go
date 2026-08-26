package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleter(t *testing.T) {
	t.Parallel()

	// a slash command at the line start is offered.
	t.Run("commands_at_line_start", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, nil)

		start, items := comp.Complete("/mo", 3)
		assert.Equal(t, 0, start)
		labels := labelsOf(items)
		assert.Contains(t, labels, "/model")
	})

	// a path after @ is completed.
	t.Run("path_after_at", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "read.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir, tools.PathPolicy{})
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, idx)

		start, items := comp.Complete("@main", 5)
		// the replacement starts just past @ so accepting keeps it
		assert.Equal(t, 1, start)
		labels := labelsOf(items)
		assert.Contains(t, labels, "main.go")
	})

	// a cursor on @ with nothing after offers no completion.
	t.Run("cursor_on_at_does_not_panic", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir, tools.PathPolicy{})
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, idx)

		// cursor sits on the @ (a break precedes it) with nothing after; no path to complete.
		_, items := comp.Complete(" @", 1)
		assert.Empty(t, items)
	})

	// completion does not trigger mid-token without a special char.
	t.Run("no_trigger_mid_token", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, nil)

		_, items := comp.Complete("hello", 5)
		assert.Empty(t, items)
	})

	// a trailing space still offers every argument candidate.
	t.Run("trailing_space_lists_all_args", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, nil)

		start, items := comp.Complete("/model ", len("/model "))
		assert.Equal(t, len("/model "), start) // replacement starts past the trailing space
		labels := labelsOf(items)
		assert.Contains(t, strings.Join(labels, "|"), "beta")
	})

	// pos is a grapheme-cell index; start must come back as cells.
	t.Run("path_non_ascii_cell_indexes", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "é.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir, tools.PathPolicy{})
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, idx)

		arg := "@é"
		pos := len(tui.GraphemeCells(arg)) // 2 cells (@ é), not byte length
		start, items := comp.Complete(arg, pos)
		assert.Equal(t, 1, start)
		labels := labelsOf(items)
		require.Contains(t, labels, "é.go")
	})

	// a command delegates argument completion to its own Complete.
	t.Run("argument_delegates", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, nil)

		// /reasoning <prefix> => delegates to reasoning's Complete, offering levels
		start, items := comp.Complete("/reasoning me", 13)
		assert.Equal(t, 11, start) // past "/reasoning "
		labels := labelsOf(items)
		assert.Contains(t, labels, "medium")
	})
}

func labelsOf(items []tui.Completion) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}
