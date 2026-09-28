package app

import (
	"context"

	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/tui"
)

// clipboardWrite is the shared clipboard write path, a var so tests fake the backend.
var clipboardWrite = clipboard.Copy

// CopyClipboard writes text through the shared clipboard writer.
func (c *uiConsole) CopyClipboard(ctx context.Context, text string) error {
	return clipboardWrite(ctx, text)
}

// copyText writes one payload to the clipboard, returning the notice to show.
func copyText(ctx context.Context, text string) (string, tui.Level, bool) {
	if err := clipboardWrite(ctx, text); err != nil {
		return err.Error(), tui.LevelError, true
	}
	return "copied to the clipboard", tui.LevelInfo, true
}
