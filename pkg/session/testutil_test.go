package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/require"
)

// ids returns each entry's id in order.
func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// msgData marshals a user text message payload for tests.
func msgData(text string) json.RawMessage {
	b, _ := json.Marshal(MessageData{Message: llm.Text(llm.RoleUser, text)})
	return b
}

// llmText builds a bare single-block message of the given role and text.
func llmText(text string) llm.Message {
	return llm.Text(llm.RoleUser, text)
}

// noticeData marshals a notice payload for tests.
func noticeData(msg string) json.RawMessage {
	b, _ := json.Marshal(NoticeData{Message: msg})
	return b
}

// loadEntries reads a committed transcript from testdata/branches. The corpus is
// frozen on purpose: every other test writes and reads in one run, so a renamed
// JSON field would change both sides at once and go unnoticed while breaking
// every session already on disk.
func loadEntries(t *testing.T, name string) []Entry {
	t.Helper()

	entries, warns, err := Read(filepath.Join("testdata", "branches", name))
	require.NoError(t, err)
	require.Empty(t, warns, "the committed transcript must still parse cleanly")
	require.NotEmpty(t, entries)
	return entries
}

// loadBranch reads a committed transcript and returns the branch ending at its
// last entry.
func loadBranch(t *testing.T, name string) []Entry {
	t.Helper()

	entries := loadEntries(t, name)
	return Branch(entries, Head(entries))
}

// blockKind names a block's type from outside pkg/llm, where blockType is
// unexported.
func blockKind(b llm.Block) string {
	switch b.(type) {
	case llm.TextBlock:
		return "text"
	case llm.ThinkingBlock:
		return "thinking"
	case llm.ToolCallBlock:
		return "tool_call"
	case llm.ToolResultBlock:
		return "tool_result"
	case llm.ImageBlock:
		return "image"
	default:
		return "unknown"
	}
}

// digest renders one line per message as "role|blockkinds|text-prefix". Asserting
// the whole shape means a silently added or dropped message fails, while the
// expectation stays readable in the test that owns it.
func digest(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		kinds := make([]string, len(m.Content))
		var text string
		for j, b := range m.Content {
			kinds[j] = blockKind(b)
			if text == "" {
				text = blockText(b)
			}
		}
		if len(text) > 40 {
			text = text[:40] + "…"
		}
		out[i] = fmt.Sprintf("%s|%s|%s", m.Role, strings.Join(kinds, ","),
			strings.ReplaceAll(text, "\n", "⏎"))
	}
	return out
}

// blockText is the leading text a block carries, if any.
func blockText(b llm.Block) string {
	switch v := b.(type) {
	case llm.TextBlock:
		return v.Text
	case llm.ThinkingBlock:
		return v.Text
	case llm.ToolCallBlock:
		return v.Name + " " + string(v.Input)
	case llm.ToolResultBlock:
		for _, cb := range v.Content {
			if tb, ok := cb.(llm.TextBlock); ok {
				return tb.Text
			}
		}
	}
	return ""
}
