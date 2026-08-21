package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHistory returns an EditorHistory rooted in a temp store for ws.
func newTestHistory(t *testing.T, ws string) *EditorHistory {
	t.Helper()
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	h, err := NewEditorHistory(s, ws, "")
	require.NoError(t, err)
	return h
}

// storedMessages reads path back as the decoded recall entries currently on disk.
func storedMessages(path string) []string { return readMessages(path) }

func TestEditorHistoryAppend(t *testing.T) {
	t.Parallel()

	t.Run("persists_on_append", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.Append("/model")
		h.Append("!ls")
		assert.Equal(t, []string{"/model", "!ls"}, storedMessages(h.path))
	})

	t.Run("multiline_message_stays_one_entry", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.Append("line one\nline two") // one turn with embedded newlines
		assert.Equal(t, []string{"line one\nline two"}, storedMessages(h.path),
			"a multi-line message is one physical row and recalls whole")
	})

	t.Run("drops_secret_prefix", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.secretPrefix = "secret:"
		h.Append("good line")
		h.Append("secret:api-key-value")
		assert.Equal(t, []string{"good line"}, storedMessages(h.path))
		assert.NotContains(t, string(mustReadFile(t, h.path)), "api-key-value", "a secret must never reach disk")
	})

	t.Run("drops_blank_and_cr", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.Append("")       // blank message is dropped entirely
		h.Append("  ")     // whitespace-only is kept (mirrors normalize)
		h.Append("text\r") // CR trimmed before writing
		assert.Equal(t, []string{"  ", "text"}, storedMessages(h.path))
	})

	t.Run("concurrent_appends_keep_all", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		const n = 40
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				h.Append(fmt.Sprintf("line-%d", i))
			}(i)
		}
		wg.Wait()

		got := storedMessages(h.path)
		assert.Len(t, got, n)
		for i := 0; i < n; i++ {
			assert.Contains(t, got, fmt.Sprintf("line-%d", i), "no concurrent message may be lost")
		}
	})

	t.Run("nil_receiver_is_noop", func(t *testing.T) {
		var h *EditorHistory
		h.Append("nothing happens") // must not panic
	})
}

func TestEditorHistoryRecent(t *testing.T) {
	t.Parallel()

	t.Run("newest_first", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		for _, l := range []string{"a", "b", "c"} {
			h.Append(l)
		}
		assert.Equal(t, []string{"c", "b", "a"}, h.Recent())
	})

	t.Run("dedup_keeps_most_recent", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		for _, l := range []string{"a", "b", "a"} {
			h.Append(l)
		}
		assert.Equal(t, []string{"a", "b"}, h.Recent())
	})

	t.Run("caps_at_max", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		for i := 0; i < maxHistoryLines+50; i++ {
			h.Append(fmt.Sprintf("line-%d", i))
		}
		assert.Len(t, h.Recent(), maxHistoryLines)
	})

	t.Run("includes_unflushed_appends", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.Append("on disk")
		h.added = append(h.added, histLine{msg: "in memory"}) // simulate an unflushed local message
		assert.Equal(t, []string{"in memory", "on disk"}, h.Recent())
	})

	t.Run("hidden_excluded_but_persisted", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.AppendHidden("/tools") // a slash command: durable yet never recalled
		h.Append("a real prompt")
		assert.Equal(t, []string{"a real prompt"}, h.Recent(), "hidden rows are absent from recall")
		raw := readHistLines(h.path) // hidden rows still land on disk and round-trip
		require.Len(t, raw, 2)
		assert.Equal(t, histLine{msg: "/tools", hidden: true}, raw[0])
	})

	t.Run("missing_file_is_empty", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir()) // no file written yet
		assert.Empty(t, h.Recent())
	})

	t.Run("caps_hand_edited_file", func(t *testing.T) {
		var b []byte
		for i := 0; i < maxHistoryLines+10; i++ { // over-long plain-text file from an external writer
			b = append(b, fmt.Sprintf("x-%d\n", i)...)
		}
		h := newTestHistory(t, t.TempDir())
		require.NoError(t, os.WriteFile(h.path, b, 0o600))
		assert.Len(t, h.Recent(), maxHistoryLines)
	})
}

// TestEditorHistoryRecentKeepsDuplicateAcrossCap guards a re-typed message surviving
// the cap: dedup keeps each text's most recent occurrence before capping.
func TestEditorHistoryRecentKeepsDuplicateAcrossCap(t *testing.T) {
	t.Parallel()

	var b []byte
	for i := 0; i < maxHistoryLines+1; i++ { // fill past the cap with unique plain rows
		b = append(b, fmt.Sprintf("line-%d\n", i)...)
	}
	b = append(b, "line-0\n"...) // re-type the oldest line most recently

	h := newTestHistory(t, t.TempDir())
	require.NoError(t, os.WriteFile(h.path, b, 0o600))
	recent := h.Recent()
	assert.Len(t, recent, maxHistoryLines)
	assert.Equal(t, "line-0", recent[0], "the most recently re-typed line is newest and survives the cap")
}

// TestEditorHistoryCompact verifies compaction rewrites the file to a merged,
// deduplicated, capped form and keeps secrets out.
func TestEditorHistoryCompact(t *testing.T) {
	t.Parallel()

	t.Run("dedups_and_caps_on_disk", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		for i := 0; i < maxHistoryLines+20; i++ {
			h.Append(fmt.Sprintf("line-%d", i))
		}
		h.Compact()
		assert.Len(t, storedMessages(h.path), maxHistoryLines)
	})

	t.Run("keeps_multiline_whole_and_secrets_out", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.secretPrefix = "secret:"
		h.Append("first line\nsecond line") // a multi-line turn survives compaction whole
		for i := 0; i < 5; i++ {
			h.Append(fmt.Sprintf("line-%d", i))
		}
		h.Append("secret:hidden-value")
		h.Compact()
		assert.Contains(t, storedMessages(h.path), "first line\nsecond line",
			"a multi-line turn is not fragmented by compaction")
		assert.NotContains(t, string(mustReadFile(t, h.path)), "hidden-value")
	})

	t.Run("merges_appends_made_after_open", func(t *testing.T) {
		h := newTestHistory(t, t.TempDir())
		h.Append("before")                                // on disk
		h.added = append(h.added, histLine{msg: "later"}) // a local message not yet flushed
		h.Compact()
		assert.Equal(t, []string{"before", "later"}, storedMessages(h.path))
	})
}

// TestEditorHistoryRecentTriggersCompaction verifies an over-long file is compacted
// off Recent's path (self-healing) without blocking the caller.
func TestEditorHistoryRecentTriggersCompaction(t *testing.T) {
	t.Parallel()

	h := newTestHistory(t, t.TempDir())
	for i := 0; i < 2*maxHistoryLines+10; i++ { // exceeds the compaction threshold
		h.Append(fmt.Sprintf("line-%d", i))
	}
	assert.Len(t, h.Recent(), maxHistoryLines) // reads capped regardless

	require.Eventually(t, func() bool {
		return len(storedMessages(h.path)) == maxHistoryLines
	}, 5*time.Second, time.Millisecond)
}

// mustReadFile returns path's bytes or fails the test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
