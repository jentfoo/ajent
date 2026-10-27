package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/refs"
)

// BenchmarkCompleter measures completion under the shapes the editor asks for:
// Style runs per keystroke under the UI lock, Complete builds the menu for a
// partial command name, its argument, and an @ path off a real directory.
func BenchmarkCompleter(b *testing.B) {
	dir := b.TempDir()
	for _, name := range []string{
		"main.go", "parse.go", "render.go", "notes.md", "Makefile",
		"parser_test.go", "reduce.go", "go.mod",
	} {
		require.NoError(b, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	require.NoError(b, os.Mkdir(filepath.Join(dir, "pkg"), 0o755))
	require.NoError(b, os.WriteFile(filepath.Join(dir, "pkg", "tui.go"), []byte("x"), 0o600))

	c := newFakeConsole(b)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)
	comp := NewCompleter(r, c, refs.NewIndex(dir))

	draft := "read the streaming code in pkg/tui and explain the repaint path\n/comp"
	for _, tc := range []struct {
		name  string
		text  string
		pos   int
		style bool
	}{
		{"style_partial_command", draft, len(draft), true},
		{"style_mid_prose", draft, 20, true},
		{"command_name", "/comp", 5, false},
		{"command_arg", "/reasoning me", 13, false},
		{"at_path", "look at @ma", 12, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			if tc.style {
				for b.Loop() {
					comp.Style(tc.text, tc.pos)
				}
				return
			}
			_, items := comp.Complete(tc.text, tc.pos)
			require.NotEmpty(b, items)

			for b.Loop() {
				_, _ = comp.Complete(tc.text, tc.pos)
			}
		})
	}
}
