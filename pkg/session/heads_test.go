package session

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveHead(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage},
		{ID: "b", ParentID: "a", Type: TypeMessage},
	}

	t.Run("no_cursor_is_tail", func(t *testing.T) {
		assert.Equal(t, "b", resolveHead(entries))
	})

	t.Run("trailing_cursor_names_parent", func(t *testing.T) {
		rewound := append(slices.Clone(entries), Entry{ID: "c", ParentID: "a", Type: TypeHead})
		assert.Equal(t, "a", resolveHead(rewound))
	})

	t.Run("trailing_new_root_cursor", func(t *testing.T) {
		forked := append(slices.Clone(entries), Entry{ID: "c", ParentID: "", Type: TypeHead})
		assert.Empty(t, resolveHead(forked))
	})

	t.Run("mid_chain_cursor_rides_tail", func(t *testing.T) {
		grew := append(slices.Clone(entries), Entry{ID: "c", ParentID: "a", Type: TypeHead},
			Entry{ID: "d", ParentID: "c", Type: TypeMessage})
		assert.Equal(t, "d", resolveHead(grew))
	})

	t.Run("empty_entries", func(t *testing.T) {
		assert.Empty(t, resolveHead(nil))
	})
}

func TestSetHeadRecordsCursor(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: Version(), Cwd: "/w"})
	require.NoError(t, err)
	e1, err := w.Append(TypeMessage, msgData("m1"))
	require.NoError(t, err)

	w.SetHead(e1.ID)

	entries, _, err := Read(p)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, TypeHead, last.Type)
	assert.Equal(t, e1.ID, last.ParentID)

	// the next append chains through the cursor onto the rewound branch
	next, err := w.Append(TypeMessage, msgData("m2"))
	require.NoError(t, err)
	assert.Equal(t, e1.ID, next.ParentID)
}

func TestOpenResolvesCursor(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: Version(), Cwd: "/w"})
	require.NoError(t, err)
	e1, err := w.Append(TypeMessage, msgData("m1"))
	require.NoError(t, err)
	_, err = w.Append(TypeMessage, msgData("m2"))
	require.NoError(t, err)
	w.SetHead(e1.ID) // rewind, then quit before anything lands
	require.NoError(t, w.Close())

	w2, entries, _, err := OpenEntries(p)
	require.NoError(t, err)
	defer func() { _ = w2.Close() }()
	assert.Equal(t, e1.ID, w2.Head())
	next, err := w2.Append(TypeMessage, msgData("m3"))
	require.NoError(t, err)
	assert.Equal(t, e1.ID, next.ParentID)

	// the reopened parse resolves the same head
	assert.Equal(t, e1.ID, resolveHead(entries))
}
