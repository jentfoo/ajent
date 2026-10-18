package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestCompleter(t *testing.T) {
	t.Parallel()

	// a slash command at the line start is offered
	t.Run("commands_at_line_start", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		start, items := comp.Complete("/mo", 3)
		assert.Equal(t, 0, start)
		labels := labelsOf(items)
		assert.Contains(t, labels, "/model")
	})

	// argument completion resolves the name as dispatch does, so an upper-case
	// command still reaches its own Complete
	t.Run("arguments_ignore_case", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		_, items := comp.Complete("/REASONING me", 13)
		assert.Contains(t, labelsOf(items), "medium")
	})

	// a command name is matched case-insensitively, as dispatch resolves it
	t.Run("commands_ignore_case", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		_, items := comp.Complete("/MO", 3)
		assert.Contains(t, labelsOf(items), "/model")
	})

	// a path after @ is completed
	t.Run("path_after_at", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "read.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir)
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, idx)

		start, items := comp.Complete("@main", 5)
		// the replacement starts just past @ so accepting keeps it
		assert.Equal(t, 1, start)
		labels := labelsOf(items)
		assert.Contains(t, labels, "main.go")
	})

	// a cursor on @ with nothing after offers no completion
	t.Run("cursor_on_at_does_not_panic", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir)
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, idx)

		// cursor sits on the @ (a break precedes it) with nothing after, so no path to complete
		_, items := comp.Complete(" @", 1)
		assert.Empty(t, items)
	})

	// completion does not trigger mid-token without a special char
	t.Run("no_trigger_mid_token", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		comp := NewCompleter(r, c, emptyIdx(t))

		_, items := comp.Complete("hello", 5)
		assert.Empty(t, items)
	})

	// a trailing space still offers every argument candidate
	t.Run("trailing_space_lists_all_args", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		start, items := comp.Complete("/model ", len("/model "))
		assert.Equal(t, len("/model "), start) // replacement starts past the trailing space
		labels := labelsOf(items)
		assert.Contains(t, strings.Join(labels, "|"), "beta")
	})

	// any run of break cells past the name is skipped, matching dispatch's
	// whitespace handling
	t.Run("args_after_multiple_breaks", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		for _, line := range []string{"/reasoning  me", "/reasoning\tme"} {
			n := len(tui.GraphemeCells(line))
			start, items := comp.Complete(line, n)
			assert.Equal(t, n-2, start, line) // replacement starts past both break cells
			assert.Contains(t, labelsOf(items), "medium", line)
		}
	})

	// pos is a grapheme-cell index, start coming back as cells
	t.Run("path_non_ascii_cell_indexes", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "é.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir)
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

	// a shell line routes to shell completion, so its @ stays literal
	t.Run("shell_line_routes_to_shell", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		idx := refs.NewIndex(dir)
		comp := NewCompleter(NewRegistry(), newFakeConsole(t), idx)

		start, items := comp.Complete("!cat mai", 8)
		assert.Equal(t, 5, start)
		assert.Equal(t, []string{"main.go"}, labelsOf(items))

		_, items = comp.Complete("!cat @mai", 9)
		assert.Empty(t, items)
	})

	// a command delegates argument completion to its own Complete
	t.Run("argument_delegates", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		comp := NewCompleter(r, c, emptyIdx(t))

		// /reasoning <prefix> => delegates to reasoning's Complete, offering levels
		start, items := comp.Complete("/reasoning me", 13)
		assert.Equal(t, 11, start) // past "/reasoning "
		labels := labelsOf(items)
		assert.Contains(t, labels, "medium")
	})
}

func TestCompleterStyle(t *testing.T) {
	t.Parallel()

	comp := NewCompleter(NewRegistry(), newFakeConsole(t), refs.NewIndex(t.TempDir()))

	cases := []struct {
		name string
		text string
		want tui.CompleteStyle
	}{
		{"at_token", "@mai", tui.CompleteStyle{Async: true}},
		{"shell_line", "!cat mai", tui.CompleteStyle{Async: true}},
		{"excluded_shell_line", "!!cat mai", tui.CompleteStyle{Async: true}},
		{"slash_command", "/mo", tui.CompleteStyle{Menu: true}},
		{"command_argument", "/model be", tui.CompleteStyle{Menu: true}},
		{"plain_text", "hello", tui.CompleteStyle{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, comp.Style(c.text, len(c.text)))
		})
	}

	// an @ token stays Tab-driven even where the workspace offers nothing
	noPaths := NewCompleter(NewRegistry(), newFakeConsole(t), emptyIdx(t))
	assert.Equal(t, tui.CompleteStyle{Async: true}, noPaths.Style("@mai", 4))
	assert.Equal(t, tui.CompleteStyle{Async: true}, noPaths.Style("!ls", 3))
}

// emptyIdx returns an index over an empty workspace, so command-focused tests
// wire a real path source that offers nothing.
func emptyIdx(t *testing.T) *refs.Index {
	t.Helper()

	return refs.NewIndex(t.TempDir())
}

func labelsOf(items []tui.Completion) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

// textsOf returns each completion's inserted text in order.
func textsOf(items []tui.Completion) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

func TestPathCompleter(t *testing.T) {
	t.Parallel()

	// a lone @ path source for free-text inputs like approval reasons
	newIdx := func(t *testing.T) refs.Index {
		t.Helper()

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "read.go"), []byte("x"), 0o600))
		return *refs.NewIndex(dir)
	}

	// an @ token anywhere completes paths, keeping the leading @ on accept
	t.Run("at_token_mid_text", func(t *testing.T) {
		idx := newIdx(t)
		text := "see @main for details"
		start, items := NewPathCompleter(&idx).Complete(text, 8)

		assert.Equal(t, 5, start) // just past @, so accepting keeps it
		assert.Contains(t, labelsOf(items), "main.go")
	})

	// a plain word offers nothing: only refs complete in free text
	t.Run("plain_words_offer_nothing", func(t *testing.T) {
		idx := newIdx(t)
		comp := NewPathCompleter(&idx)

		start, items := comp.Complete("read main for details", 8)
		assert.Empty(t, items)
		assert.Equal(t, 8, start)

		start, items = comp.Complete("@main done", 10) // cursor off the token
		assert.Empty(t, items)
		assert.Equal(t, 10, start)
	})

	// a cursor right after @ with an empty query lists the directory
	t.Run("empty_query_lists_children", func(t *testing.T) {
		idx := newIdx(t)

		start, items := NewPathCompleter(&idx).Complete("@", 1)

		assert.Equal(t, 1, start)
		assert.ElementsMatch(t, labelsOf(items), []string{"main.go", "read.go"})
	})
}

func TestPathCompleterMatchesPrompt(t *testing.T) {
	t.Parallel()

	// the free-text @ source must answer exactly what the prompt's does, ../
	// forms included: both route one shared core, pinned equal here
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "other"), 0o755))
	require.NoError(t, os.MkdirAll(proj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "foo.txt"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other", "bar.txt"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "main.go"), []byte("x"), 0o600))
	idx := refs.NewIndex(proj)

	r := NewRegistry()
	c := newFakeConsole(t)
	prompt := NewCompleter(r, c, idx)
	notes := NewPathCompleter(idx)

	// every @ shape a user can type, cursor at the token's end
	for _, tc := range []struct {
		text string
		pos  int
	}{
		{"@m", 2},
		{"@", 1},
		{"@../f", 5},
		{"@../foo", 6},
		{"@../", 4},
		{"@../other/", 10},
		{"@../other/b", 11},
		{"see @../other/ here", 14},
		{"@~/", 3},
		{"@nope", 5},
		{"plain words", 5},
	} {
		pStart, pItems := prompt.Complete(tc.text, tc.pos)
		nStart, nItems := notes.Complete(tc.text, tc.pos)
		assert.Equal(t, pStart, nStart, "start differs for %q at %d", tc.text, tc.pos)
		assert.Equal(t, pItems, nItems, "items differ for %q at %d", tc.text, tc.pos)
	}

	// the restriction holds: free text never completes commands, only @ refs
	start, items := notes.Complete("/mod", 4)
	assert.Empty(t, items)
	assert.Equal(t, 4, start)
}
