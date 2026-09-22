package app

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyText(t *testing.T) {
	t.Parallel()

	t.Run("success_reports_notice", func(t *testing.T) {
		var got []string
		real := clipboardWrite
		clipboardWrite = func(_ context.Context, text string) error { got = append(got, text); return nil }
		t.Cleanup(func() { clipboardWrite = real })

		notice, level, ok := copyText("payload")
		assert.True(t, ok)
		assert.Equal(t, "copied to the clipboard", notice)
		assert.Equal(t, tui.LevelInfo, level)
		assert.Equal(t, []string{"payload"}, got)
	})

	t.Run("failure_names_the_backend_problem", func(t *testing.T) {
		real := clipboardWrite
		clipboardWrite = func(context.Context, string) error { return errors.New("no clipboard writer on PATH") }
		t.Cleanup(func() { clipboardWrite = real })

		notice, level, ok := copyText("payload")
		assert.True(t, ok)
		assert.Equal(t, "no clipboard writer on PATH", notice)
		assert.Equal(t, tui.LevelError, level)
	})
}

// TestCopySelectionNoPicker pins the silent no-op: a ctrl+x that outlived its
// picker copies nothing and reports nothing.
func TestCopySelectionNoPicker(t *testing.T) {
	t.Parallel()

	ui := plainTestUI(t)
	_, ok := ui.CopySelection()
	assert.False(t, ok)
}

// plainTestUI opens a plain-mode UI over pipes, where no picker can be active.
func plainTestUI(t *testing.T) *tui.UI {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	go func() { _, _ = io.Copy(io.Discard, outR) }() // output writes must not fill the pipe

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)
	return ui
}
