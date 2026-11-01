package app

import (
	"context"
	"errors"

	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/tui"
)

// clipboardWrite is the shared clipboard write path, a var so tests fake the backend.
var clipboardWrite = clipboard.Copy

// CopyClipboard writes text through the shared clipboard writer, OSC 52
// escapes serialized with the TUI's own frames. Serve mode has no terminal to
// write escapes to, so it refuses rather than dereference a nil one.
func (c *uiConsole) CopyClipboard(ctx context.Context, text string) error {
	if !c.ui.Interactive() {
		return errors.New("no terminal to copy from")
	}
	return clipboardWrite(ctx, text, clipboard.CopyOptions{TTY: c.ui.TerminalWriter()})
}

// copyText writes text through the shared clipboard writer, riding ui's terminal
// for OSC 52 delivery on remote terminals. It returns the notice to show with its
// level and whether the caller should notify.
func copyText(ctx context.Context, ui *hubUI, text string) (string, tui.Level, bool) {
	if err := clipboardWrite(ctx, text, clipboard.CopyOptions{TTY: ui.TerminalWriter()}); err != nil {
		return err.Error(), tui.LevelError, true
	}
	return "copied to the clipboard", tui.LevelInfo, true
}
