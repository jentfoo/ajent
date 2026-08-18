package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRecallIndex builds a RecallIndex over a fresh store and history for ws.
func newRecallIndex(t *testing.T) (*Store, string, *EditorHistory, *RecallIndex) {
	t.Helper()
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir() // transcripts must live under the same workspace as recall
	h, err := NewEditorHistory(s, ws, "")
	require.NoError(t, err)
	return s, ws, h, NewRecallIndex(s, ws, h)
}

// recordPrompt appends one user text message to a fresh transcript in store for ws.
func recordPrompt(t *testing.T, s *Store, ws, txt string) {
	t.Helper()
	w, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, txt)})
	require.NoError(t, aerr)
}

func TestRecallIndexLines(t *testing.T) {
	t.Parallel()

	t.Run("typed_lines_first", func(t *testing.T) {
		_, _, h, idx := newRecallIndex(t)
		h.Append("/model")
		h.Append("!ls")
		assert.Equal(t, []string{"!ls", "/model"}, recallTexts(idx.Lines()))
	})

	t.Run("prompts_backfill_older", func(t *testing.T) {
		s, ws, _, idx := newRecallIndex(t)
		recordPrompt(t, s, ws, "recorded prompt")
		recordPrompt(t, s, ws, "also recorded")
		assert.Equal(t, []string{"also recorded", "recorded prompt"}, recallTexts(idx.Lines()))
	})

	t.Run("dedup_across_sources", func(t *testing.T) {
		s, ws, h, idx := newRecallIndex(t)
		h.Append("shared line") // typed recently
		recordPrompt(t, s, ws, "old prompt")
		recordPrompt(t, s, ws, "shared line")
		assert.Equal(t, []string{"shared line", "old prompt"}, recallTexts(idx.Lines()),
			"a typed-then-recorded line appears once")
	})

	t.Run("empty_sources_is_empty", func(t *testing.T) {
		s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
		ws := t.TempDir()
		h, err := NewEditorHistory(s, ws, "")
		require.NoError(t, err)
		idx := NewRecallIndex(s, ws, h)
		assert.Empty(t, idx.Lines())
	})
}

// TestRecallIndexTimestampFromPrompt pins the clock so a typed-and-recorded line
// proves it keeps the prompt's timestamp. Sequential: setClock mutates a package globals.
func TestRecallIndexTimestampFromPrompt(t *testing.T) {
	s, ws, h, idx := newRecallIndex(t)
	h.Append("shared line") // no timestamp of its own
	t.Cleanup(setClock(time.UnixMilli(1_700_000_001).UTC()))
	recordPrompt(t, s, ws, "shared line")

	for _, p := range idx.Lines() {
		if p.Text == "shared line" {
			assert.False(t, p.At.IsZero(), "a typed line that was also recorded keeps its timestamp")
		}
	}
}

// recallTexts returns each recalled line's text in order.
func recallTexts(ps []Prompt) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text
	}
	return out
}
