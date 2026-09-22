package app

import (
	"context"

	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/tui"
)

// clipboardWrite is the shared clipboard write path; a var so tests fake the backend.
var clipboardWrite = clipboard.Copy

// CopyClipboard writes text through the shared clipboard writer.
func (c *uiConsole) CopyClipboard(ctx context.Context, text string) error {
	return clipboardWrite(ctx, text)
}

// copySelection copies the highlighted context-tree row, the ctrl+x path. ok is
// false when no picker row carried a payload, which is silent.
func copySelection(ui *tui.UI) (notice string, level tui.Level, ok bool) {
	text, ok := ui.CopySelection()
	if !ok {
		return "", tui.LevelInfo, false
	}
	return copyText(text)
}

// copyText writes one payload to the clipboard, returning the notice to show.
func copyText(text string) (string, tui.Level, bool) {
	if err := clipboardWrite(context.Background(), text); err != nil {
		return err.Error(), tui.LevelError, true
	}
	return "copied to the clipboard", tui.LevelInfo, true
}
