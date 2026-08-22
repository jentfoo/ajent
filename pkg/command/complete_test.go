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

func TestCompleterCommandsAtLineStart(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)
	comp := NewCompleter(r, c, nil)

	start, items := comp.Complete("/mo", 3)
	assert.Equal(t, 0, start)
	labels := labelsOf(items)
	assert.Contains(t, labels, "/model")
}

func TestCompleterPathAfterAt(t *testing.T) {
	t.Parallel()

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
}

func TestCompleterCursorOnAtDoesNotPanic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o600))
	idx := refs.NewIndex(dir, tools.PathPolicy{})
	c := newFakeConsole(t)
	r := NewRegistry()
	comp := NewCompleter(r, c, idx)

	// cursor sits on the @ (a break precedes it) with nothing after; no path to complete.
	_, items := comp.Complete(" @", 1)
	assert.Empty(t, items)
}

func TestCompleterNoTriggerMidToken(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	comp := NewCompleter(r, c, nil)

	_, items := comp.Complete("hello", 5)
	assert.Empty(t, items)
}

func TestCompleterTrailingSpaceListsAllArgs(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)
	comp := NewCompleter(r, c, nil)

	// a command line ending in a space still offers every argument candidate
	start, items := comp.Complete("/model ", len("/model "))
	assert.Equal(t, len("/model "), start) // replacement starts past the trailing space
	labels := labelsOf(items)
	assert.Contains(t, strings.Join(labels, "|"), "beta")
}

func TestCompleterPathNonASCIICellIndexes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "é.go"), []byte("x"), 0o600))
	idx := refs.NewIndex(dir, tools.PathPolicy{})
	c := newFakeConsole(t)
	r := NewRegistry()
	comp := NewCompleter(r, c, idx)

	// pos is a grapheme-cell index (bytes differ); start must come back as cells.
	arg := "@é"
	pos := len(tui.GraphemeCells(arg)) // 2 cells (@ é), not byte length
	start, items := comp.Complete(arg, pos)
	assert.Equal(t, 1, start)
	labels := labelsOf(items)
	require.Contains(t, labels, "é.go")
}

func TestCompleterArgumentDelegates(t *testing.T) {
	t.Parallel()

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
}

func labelsOf(items []tui.Completion) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}
