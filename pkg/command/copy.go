package command

import (
	"context"
	"strconv"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/llm"
)

// copyCommand puts the last agent response on the clipboard: text plus every
// tool call and its result as verbatim JSON, through the shared clipboard
// writer. No agent messages yet is a notice, not an error.
func copyCommand(ctx context.Context, _ string, c Console) error {
	st := c.State()
	if st == nil {
		c.Notify("No agent messages to copy", levelWarn)
		return nil
	}
	text := llm.CopyTurn(st.Messages)
	if text == "" {
		c.Notify("No agent messages to copy", levelWarn)
		return nil
	}
	if err := c.CopyClipboard(ctx, text); err != nil {
		c.Notify(err.Error(), levelError)
		return nil
	}
	c.Notify("copied "+strconv.Itoa(utf8.RuneCountInString(text))+" chars to the clipboard", levelInfo)
	return nil
}
