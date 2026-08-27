package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellComplete(t *testing.T) {
	t.Parallel()

	// paths complete relative to the workspace root, one directory at a time
	t.Run("path_argument", func(t *testing.T) {
		comp := newShellCompleter(t, "pkg")

		start, items := comp.Complete("!cat pk", 7)
		assert.Equal(t, 5, start)
		assert.Equal(t, []string{"pkg/"}, labelsOf(items))
	})

	// `!!` shifts the command text by one more cell
	t.Run("excluded_run_offset", func(t *testing.T) {
		comp := newShellCompleter(t, "pkg")

		start, items := comp.Complete("!!cat pk", 8)
		assert.Equal(t, 6, start)
		assert.Equal(t, []string{"pkg/"}, labelsOf(items))
	})

	// a first word holding a separator is a path, as bash treats ./script
	t.Run("first_word_with_slash", func(t *testing.T) {
		comp := newShellCompleter(t, "pkg")

		_, items := comp.Complete("!./pk", 5)
		assert.Equal(t, []string{"./pkg/"}, labelsOf(items))
	})

	// an @ inside a shell line is literal text for bash, never a workspace ref
	t.Run("at_is_not_a_ref", func(t *testing.T) {
		comp := newShellCompleter(t, "pkg")

		_, items := comp.Complete("!cat @pk", 8)
		assert.Empty(t, items)
	})

	// a bare ! offers nothing; every command on the system is not a useful list
	t.Run("empty_command_token", func(t *testing.T) {
		comp := newShellCompleter(t, "pkg")

		_, items := comp.Complete("!", 1)
		assert.Empty(t, items)
	})

	t.Run("command_names_after_bang", func(t *testing.T) {
		requireBash(t)
		comp := newShellCompleter(t, "pkg")

		_, items := comp.Complete("!ech", 4)
		assert.Contains(t, labelsOf(items), "echo")
	})

	// a separator restarts the command position mid-line
	t.Run("command_names_after_pipe", func(t *testing.T) {
		requireBash(t)
		comp := newShellCompleter(t, "pkg")

		for _, line := range []string{"!ls | ech", "!ls && ech", "!ls; ech"} {
			_, items := comp.Complete(line, len(line))
			assert.Contains(t, labelsOf(items), "echo", line)
		}
	})
}

func TestShellCmdStart(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1, shellCmdStart(tui.GraphemeCells("!ls")))
	assert.Equal(t, 2, shellCmdStart(tui.GraphemeCells("!!ls")))
	assert.Equal(t, 1, shellCmdStart(tui.GraphemeCells("!")))
}

func TestIsCmdPosition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want bool
	}{
		{"first_word", "!ls", true},
		{"after_space", "! ls", true},
		{"second_word", "!ls foo", false},
		{"after_pipe", "!ls | gr", true},
		{"after_and", "!ls && gr", true},
		{"after_semicolon", "!ls; gr", true},
		{"after_subshell", "!(gr", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cells := tui.GraphemeCells(c.line)
			from := shellCmdStart(cells)
			start := len(cells)
			for start > from && !isTokenBreakCell(cells[start-1]) {
				start--
			}
			assert.Equal(t, c.want, isCmdPosition(cells, from, start))
		})
	}
}

func TestShellNames(t *testing.T) {
	t.Parallel()
	requireBash(t)

	names := shellNames()
	require.NotEmpty(t, names)
	assert.Contains(t, names, "cd", "bash builtins belong in the list")
	assert.True(t, slices.IsSorted(names))
	assert.NotContains(t, names, "")
}

// newShellCompleter returns a completer rooted at a temp dir holding dirs.
func newShellCompleter(t *testing.T, dirs ...string) *Completer {
	t.Helper()
	dir := t.TempDir()
	for _, d := range dirs {
		require.NoError(t, os.Mkdir(filepath.Join(dir, d), 0o750))
	}
	return NewCompleter(NewRegistry(), newFakeConsole(t), refs.NewIndex(dir, tools.PathPolicy{Cwd: dir}))
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
}
