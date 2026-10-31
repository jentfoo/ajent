package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slotEntries builds one dead slot naming head with a watermark of n entries.
func slotEntries(t *testing.T, path, token, head string, entryTS int64, n int) headSlot {
	t.Helper()
	s := headSlot{Token: token, Head: head, EntryTS: entryTS, Entries: n,
		PID: os.Getpid(), Started: clock().UnixMilli()}
	require.NoError(t, writeSlot(path, s))
	return s
}

func TestHeadForEntries(t *testing.T) {
	t.Parallel()

	// root -> a -> b, b is the file tail
	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
		{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")},
	}

	t.Run("no_slots_falls_to_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		assert.Equal(t, "b", headForEntries(p, entries, true))
	})

	t.Run("dead_slot_head_beats_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		slotEntries(t, p, "t1", "a", 5, len(entries)) // rewind onto a, nothing grew since
		assert.Equal(t, "a", headForEntries(p, entries, true))
	})

	t.Run("unsynced_tail_extends_slot", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		// the slot was written before b landed, and b chains back to the head
		slotEntries(t, p, "t1", "a", 5, 2)
		assert.Equal(t, "b", headForEntries(p, entries, true))
	})

	t.Run("forked_growth_keeps_slot_head", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		// growth that breaks the parent chain is another tip's fork, not this
		// writer's unsynced work: rewind onto b, then a sibling branches from a
		forked := append(slices.Clone(entries), Entry{ID: "c", ParentID: "a", Type: TypeMessage})
		slotEntries(t, p, "t1", "b", 9, len(entries))
		assert.Equal(t, "b", headForEntries(p, forked, true))
	})

	t.Run("unresolvable_id_falls_to_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		slotEntries(t, p, "t1", "gone", 5, len(entries))
		assert.Equal(t, "b", headForEntries(p, entries, true))
	})

	t.Run("newest_dead_slot_wins", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		slotEntries(t, p, "old", "a", 5, len(entries))
		slotEntries(t, p, "new", "b", 9, len(entries)) // newer head stamp
		assert.Equal(t, "b", headForEntries(p, entries, true))
	})

	// a new-root fork carries no entry stamp, so slot write time orders it
	t.Run("empty_head_orders_by_write_time", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		slotEntries(t, p, "old", "a", 900, len(entries))
		// backdate deterministically: two writes can land in one mtime tick
		past := time.Now().Add(-time.Minute)
		require.NoError(t, os.Chtimes(slotPath(headsDir(p), "old"), past, past))
		slotEntries(t, p, "fork", "", 0, len(entries))
		assert.Empty(t, headForEntries(p, entries, true))
	})

	t.Run("live_slot_adopts_implicit", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		s := slotEntries(t, p, "live", "a", 5, len(entries))
		s.Active = true
		require.NoError(t, writeSlot(p, s))
		assert.Equal(t, "a", headForEntries(p, entries, false))
	})

	// an empty live head is a mid-fork or fresh writer with no branch to adopt:
	// adoption falls through to the next newest live peer instead of forking a
	// rootless sibling
	t.Run("empty_head_skips_next_peer", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		s := slotEntries(t, p, "concrete", "a", 5, len(entries))
		s.Active = true
		require.NoError(t, writeSlot(p, s))
		// backdate deterministically: two writes can land in one mtime tick
		past := time.Now().Add(-time.Minute)
		require.NoError(t, os.Chtimes(slotPath(headsDir(p), "concrete"), past, past))
		f := slotEntries(t, p, "midfork", "", 0, len(entries))
		f.Active = true
		require.NoError(t, writeSlot(p, f))
		assert.Equal(t, "a", headForEntries(p, entries, false))
	})

	t.Run("explicit_open_skips_live_slot", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		s := slotEntries(t, p, "live", "a", 5, len(entries))
		s.Active = true
		require.NoError(t, writeSlot(p, s))
		assert.Equal(t, "b", headForEntries(p, entries, true))
	})

	// a sibling session's slots must not steer this one
	t.Run("ignores_sibling_slots", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		slotEntries(t, p1, "t1", "a", 5, len(entries))
		e2 := []Entry{{ID: "y", Type: TypeSession}}
		assert.Equal(t, "y", headForEntries(p2, e2, true))
	})
}

func TestMigrateLegacyHead(t *testing.T) {
	t.Parallel()

	t.Run("seeds_dead_slot_removes_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, os.WriteFile(p+legacyHeadSuffix, []byte(`{"id":"a","n":3}`), 0o600))

		migrateLegacyHead(p)

		assert.False(t, fileExists(p+legacyHeadSuffix))
		slots := readSlots(p)
		require.Len(t, slots, 1)
		assert.Equal(t, "a", slots[0].Head)
		assert.False(t, slots[0].Active)
		assert.Equal(t, 3, slots[0].Entries) // the watermark rides along
	})

	t.Run("unreadable_cursor_dropped", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, os.WriteFile(p+legacyHeadSuffix, []byte("garbage"), 0o600))

		migrateLegacyHead(p)

		assert.False(t, fileExists(p+legacyHeadSuffix))
		assert.Empty(t, readSlots(p))
	})

	t.Run("no_cursor_no_slots", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		migrateLegacyHead(p)
		assert.Empty(t, readSlots(p))
	})

	// a synced cursor naming a mid-file rewind with the full count keeps the
	// rewind: a dropped watermark would read the growth as unsynced and adopt
	// the tail instead
	t.Run("watermark_keeps_mid_file_rewind", func(t *testing.T) {
		entries := []Entry{
			{ID: "root", Type: TypeSession},
			{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
			{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")},
			{ID: "c", ParentID: "b", Type: TypeMessage, Data: msgData("m3")},
			{ID: "d", ParentID: "c", Type: TypeMessage, Data: msgData("m4")},
		}
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, os.WriteFile(p+legacyHeadSuffix,
			[]byte(`{"id":"b","n":5}`), 0o600))

		migrateLegacyHead(p)

		assert.Equal(t, "b", headForEntries(p, entries, true))
	})
}

func TestLiveWriters(t *testing.T) {
	t.Parallel()

	t.Run("counts_active_fresh_slots", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		s := slotEntries(t, p, "t1", "a", 5, 3)
		s.Active = true
		require.NoError(t, writeSlot(p, s))
		assert.Equal(t, 1, LiveWriters(p))
	})

	t.Run("inactive_stale_do_not_count", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		slotEntries(t, p, "dead", "a", 5, 3) // written inactive: a clean exit
		assert.Equal(t, 0, LiveWriters(p))

		stale := headSlot{Token: "crashed", Head: "a", Active: true}
		require.NoError(t, writeSlot(p, stale))
		// backdate past the freshness window without touching the file again
		past := time.Now().Add(-2 * slotStaleAfter)
		require.NoError(t, os.Chtimes(slotPath(headsDir(p), "crashed"), past, past))
		assert.Equal(t, 0, LiveWriters(p))
	})
}

func TestSlotLive(t *testing.T) {
	t.Parallel()

	now := time.UnixMilli(1_800_000_000_000)
	fresh := now.Add(-time.Second)
	stale := now.Add(-2 * slotStaleAfter)

	t.Run("active_is_live", func(t *testing.T) {
		assert.True(t, slotLive(headSlot{Active: true}, fresh, now))
	})

	t.Run("clean_exit_dead", func(t *testing.T) {
		assert.False(t, slotLive(headSlot{Active: false}, fresh, now))
	})

	t.Run("crashed_writer_stale", func(t *testing.T) {
		assert.False(t, slotLive(headSlot{Active: true}, stale, now))
	})
}

func TestGcStaleSlots(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	crashed := headSlot{Token: "crashed", Head: "a", Active: true}
	require.NoError(t, writeSlot(p, crashed))
	past := time.Now().Add(-2 * slotStaleAfter)
	crashedPath := slotPath(headsDir(p), "crashed")
	require.NoError(t, os.Chtimes(crashedPath, past, past))
	keep := headSlot{Token: "kept", Head: "a"} // inactive: the resume cursor
	require.NoError(t, writeSlot(p, keep))

	gcStaleSlots(p, time.Now())

	assert.False(t, fileExists(crashedPath))
	assert.True(t, fileExists(slotPath(headsDir(p), "kept")))
}

func TestRemoveSlotsDropsDirectory(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	slotEntries(t, p, "t1", "a", 5, 3)
	require.True(t, fileExists(headsDir(p)))

	removeSlots(p)

	assert.False(t, fileExists(headsDir(p)))
}

func TestPruneDeadSlotsKeepsNewest(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	for i, ts := range []int64{1, 2, 3, 4, 5, 6, 7} {
		slotEntries(t, p, fmt.Sprintf("t%d", i), fmt.Sprintf("h%d", i), ts, 3)
	}
	live := slotEntries(t, p, "live", "h7", 9, 3)
	live.Active = true
	require.NoError(t, writeSlot(p, live))

	pruneDeadSlots(p)

	slots := readSlots(p)
	require.Len(t, slots, keepDeadSlots+1) // newest dead plus the live one
	names := make([]string, 0, len(slots))
	for _, s := range slots {
		names = append(names, s.Token)
	}
	assert.Contains(t, names, "live")
	assert.Contains(t, names, "t6") // newest dead kept
	assert.NotContains(t, names, "t0")
	assert.NotContains(t, names, "t2")
}
