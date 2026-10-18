package app

import (
	"context"

	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/tui"
)

// clipboardWrite is the shared clipboard write path, a var so tests fake the backend.
var clipboardWrite = clipboard.Copy

// CopyClipboard writes text through the shared clipboard writer, OSC 52
// escapes serialized with the TUI's own frames.
func (c *uiConsole) CopyClipboard(ctx context.Context, text string) error {
	return clipboardWrite(ctx, text, clipboard.CopyOptions{TTY: c.ui.TerminalWriter()})
}

// copyText writes text through the shared clipboard writer, riding ui's terminal
// for OSC 52 delivery on remote terminals. It returns the notice to show with its
// level and whether the caller should notify.
func copyText(ctx context.Context, ui *tui.UI, text string) (string, tui.Level, bool) {
	if err := clipboardWrite(ctx, text, clipboard.CopyOptions{TTY: ui.TerminalWriter()}); err != nil {
		return err.Error(), tui.LevelError, true
	}
	return "copied to the clipboard", tui.LevelInfo, true
}
