package compact

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/require"
)

// builders shared by every test file in the package.

func msg(id string, m llm.Message) session.Entry {
	b, _ := json.Marshal(session.MessageData{Message: m})
	return session.Entry{ID: id, Type: session.TypeMessage, Data: b}
}

func userText(id, text string) session.Entry { return msg(id, llm.Text(llm.RoleUser, text)) }

func assistText(id, text string) session.Entry { return msg(id, llm.Text(llm.RoleAssistant, text)) }

func callMsg(id, callID, name, path string) session.Entry {
	input := json.RawMessage(`{}`)
	if path != "" {
		input = json.RawMessage(`{"path":"` + path + `"}`)
	}
	m := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: callID, Name: name, Input: input},
	}}
	return msg(id, m)
}

func resultMsg(id, callID, text string, isError bool) session.Entry {
	m := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: callID, IsError: isError,
			Content: llm.BlockList{llm.TextBlock{Text: text}}},
	}}
	return msg(id, m)
}

func compactEntry(id, summary, firstKept string) session.Entry {
	b, _ := json.Marshal(session.CompactionData{Summary: summary, FirstKeptEntryID: firstKept})
	return session.Entry{ID: id, Type: session.TypeCompaction, Data: b}
}

// textOf returns the first text block of a message.
func textOf(m llm.Message) string {
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// loadFixtureBranch reads a committed transcript from the session package's
// corpus. The path reaches across because both loaders are test-only, so the
// fixtures cannot be shared through an exported helper.
func loadFixtureBranch(t *testing.T, name string) []session.Entry {
	t.Helper()

	entries, warns, err := session.Read(filepath.Join("..", "session", "testdata", "branches", name))
	require.NoError(t, err)
	require.Empty(t, warns)
	return session.Branch(entries, session.Head(entries))
}
