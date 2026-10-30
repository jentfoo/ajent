package session

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/paths"
)

// Per-writer head slots replace the single `.head` cursor: every open-for-append
// owns one `<transcript>.heads/<token>.json` file written by atomic rename, so
// concurrent writers never overwrite each other's branch and deletion can see a
// live peer from its first possible write. The slot directory is per transcript,
// not per workspace, so a sibling session's Remove never touches another
// session's slots and the whole directory is one transcript's sidecar. Liveness
// is the active flag plus slot mtime freshness; a clean exit flips the flag at
// once so exit never blocks --delete for the freshness window.
const (
	headsDirSuffix = ".heads"

	// slotTouch is the background rewrite cadence keeping a live slot fresh.
	slotTouch = time.Minute

	// slotStaleAfter bounds liveness by mtime: a writer silent this long (a crash,
	// or a laptop asleep past the touch) reads as gone until its next touch.
	slotStaleAfter = 10 * time.Minute

	// keepDeadSlots bounds how many inactive slots survive a prune: the newest
	// few are resume cursors, older ones are history nothing reads.
	keepDeadSlots = 4

	// legacyHeadSuffix names the pre-slot cursor sidecar, read once for migration.
	legacyHeadSuffix = ".head"
)

// headSlot is one writer's branch head beside its transcript. An empty Head
// names a new-root fork and orders by slot write time instead of EntryTS.
// Entries is the transcript's entry count at the last write, this writer's own
// watermark: growth past it in one parent chain from Head is unsynced work, not
// a fork, and slots are sidecars rather than entries so the json rules for
// those never apply here.
type headSlot struct {
	Token   string `json:"token"`
	Head    string `json:"head,omitempty"`
	EntryTS int64  `json:"entryTs,omitempty"` // head entry stamp, primary slot order
	Entries int    `json:"entries,omitempty"`
	PID     int    `json:"pid"`
	Started int64  `json:"started"` // unix millis, for registry and banners
	Active  bool   `json:"active"`
}

// headsDir returns a transcript's writer-slot directory.
func headsDir(sessionPath string) string { return sessionPath + headsDirSuffix }

// slotPath returns one token's slot file under dir.
func slotPath(dir, token string) string { return filepath.Join(dir, token+".json") }

// writeSlot atomically rewrites one slot file, creating the directory when a
// concurrent Remove won it between open and write.
func writeSlot(sessionPath string, s headSlot) error {
	dir := headsDir(sessionPath)
	if err := os.MkdirAll(dir, paths.DirPerm); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(slotPath(dir, s.Token), b, config.SecretPerm)
}

// readSlots returns every decodable slot beside the transcript, newest last.
// A corrupt file reads as absent: a slot is a cache of live state, never truth.
func readSlots(sessionPath string) []headSlot {
	dir := headsDir(sessionPath)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []headSlot
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			continue
		}
		var s headSlot
		if json.Unmarshal(b, &s) != nil || s.Token == "" || s.Token != strings.TrimSuffix(f.Name(), ".json") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// slotLive reports whether s counts as a live peer as of now, given its file's
// mtime. An inactive slot is a clean exit, never live however fresh.
func slotLive(s headSlot, mod time.Time, now time.Time) bool {
	return s.Active && now.Sub(mod) < slotStaleAfter
}

// slotFresh is the read-side twin of writeSlot: the slot plus its file mtime.
type slotFresh struct {
	slot headSlot
	mod  time.Time
}

// readFreshSlots pairs every slot with its mtime, the freshness and tie-break
// clock.
func readFreshSlots(sessionPath string) []slotFresh {
	var out []slotFresh
	for _, s := range readSlots(sessionPath) {
		fi, err := os.Stat(slotPath(headsDir(sessionPath), s.Token))
		if err != nil {
			continue
		}
		out = append(out, slotFresh{slot: s, mod: fi.ModTime()})
	}
	return out
}

// gcStaleSlots removes crashed writers' slot files: ones still marked active
// whose mtime passed the freshness window. A suspended writer's file may be
// removed racily, which is benign, it rewrites on its next touch. Inactive slots
// stay for pruneDeadSlots, which bounds them.
func gcStaleSlots(sessionPath string, now time.Time) {
	for _, sf := range readFreshSlots(sessionPath) {
		if sf.slot.Active && now.Sub(sf.mod) >= slotStaleAfter {
			_ = os.Remove(slotPath(headsDir(sessionPath), sf.slot.Token))
		}
	}
}

// newerSlot orders two slots newest first: head entry timestamp when both name
// a real entry, slot write time otherwise (a new-root fork has no entry to
// stamp, and the write time is what the overview pins it to), so equal clocks
// still resolve deterministically.
func newerSlot(a, b slotFresh) int {
	if a.slot.Head != "" && b.slot.Head != "" {
		if c := cmp.Compare(b.slot.EntryTS, a.slot.EntryTS); c != 0 {
			return c
		}
	}
	return b.mod.Compare(a.mod)
}

// pruneDeadSlots drops inactive slots beyond the newest few, so the directory
// holds live writers and recent cursors rather than one file per historical
// open. Only writer opens prune: readers never mutate.
func pruneDeadSlots(sessionPath string) {
	dead := bulk.SliceFilter(func(sf slotFresh) bool { return !sf.slot.Active },
		readFreshSlots(sessionPath))
	if len(dead) <= keepDeadSlots {
		return
	}
	slices.SortStableFunc(dead, newerSlot)
	for _, sf := range dead[keepDeadSlots:] {
		_ = os.Remove(slotPath(headsDir(sessionPath), sf.slot.Token))
	}
}

// LiveWriters counts a transcript's live writer slots: the peers a --delete must
// name, and the gate that keeps a shared session's file from vanishing under a
// running writer. Read errors count as no peers, matching the store's
// best-effort sidecar handling.
func LiveWriters(sessionPath string) int {
	now := clock()
	return len(bulk.SliceFilter(func(sf slotFresh) bool { return slotLive(sf.slot, sf.mod, now) },
		readFreshSlots(sessionPath)))
}

// legacyCursor is the pre-slot cursor shape, parsed only to migrate it.
type legacyCursor struct {
	ID string `json:"id"`
	N  int    `json:"n,omitempty"`
}

// migrateLegacyHead seeds the pre-slot cursor as one dead slot and removes the
// cursor file, so a session saved by an older build resumes from the same head.
// Two first-openers seeding concurrently is benign: same head, distinct tokens,
// and the cursor removal tolerates ENOENT.
func migrateLegacyHead(sessionPath string) {
	b, err := os.ReadFile(sessionPath + legacyHeadSuffix)
	if err != nil {
		return
	}
	var cur legacyCursor
	if json.Unmarshal(b, &cur) != nil || cur.ID == "" {
		_ = os.Remove(sessionPath + legacyHeadSuffix) // unreadable cursor is clutter
		return
	}
	_ = writeSlot(sessionPath, headSlot{
		Token: NewID(), Head: cur.ID, Entries: cur.N, PID: os.Getpid(), Started: clock().UnixMilli(),
	})
	_ = os.Remove(sessionPath + legacyHeadSuffix)
}

// resolveOpenHead prepares a transcript for a writer open, then applies the
// multi-writer head order: the legacy cursor is seeded into a dead slot first
// (so a session saved by an older build resumes where it left off, watermark
// included), crashed writers' stale files GC'd, dead slots pruned to the newest
// few, then the slot order runs.
func resolveOpenHead(path string, entries []Entry, explicit bool) (string, int64) {
	migrateLegacyHead(path)
	gcStaleSlots(path, clock())
	pruneDeadSlots(path)
	head := headForEntries(path, entries, explicit)
	if head == "" {
		return "", 0
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.ID == head })
	if i < 0 {
		return head, 0 // slot names an entry the file no longer holds; mtime orders the slot
	}
	return head, entries[i].TS
}

// headForEntries resolves the branch head one transcript continues from, in the
// multi-writer order: a live peer's head first (an implicit resume forks from
// the tip the live writer sits at), then the newest dead slot's head extended
// to the file tail when the tail chains back to it (per-writer crash recovery,
// sound under interleaving because parent chains ignore file order), then the
// file tail. An empty live head names a mid-fork or fresh writer with no branch
// to adopt, so adoption skips it for the next newest peer. An explicit open
// skips live adoption so the session the caller named is the one that opens,
// while crash recovery still applies. Read-only: listing and prompt scans
// share it and never touch slot files.
func headForEntries(sessionPath string, entries []Entry, explicit bool) string {
	now := clock()
	live, dead := bulk.SliceSplit(func(sf slotFresh) bool { return slotLive(sf.slot, sf.mod, now) },
		readFreshSlots(sessionPath))
	if !explicit {
		slices.SortStableFunc(live, newerSlot)
		for _, sf := range live {
			if sf.slot.Head != "" {
				return sf.slot.Head
			}
		}
	}
	slices.SortStableFunc(dead, newerSlot)
	if len(dead) > 0 {
		if head, ok := extendDeadHead(entries, dead[0].slot); ok {
			return head
		}
	}
	return Head(entries)
}

// extendDeadHead applies one dead slot's unsynced-tail rule. The slot head
// stands unless the file grew past that slot's own watermark and everything
// after the head chains back to it in one parent run, which is the writer's
// unsynced work, not a fork. Interleaved peers break the chain and are skipped
// rather than adopted, so their work stays rewind-reachable; a head id the file
// no longer holds yields no decision and falls through to tail recovery.
func extendDeadHead(entries []Entry, s headSlot) (string, bool) {
	if s.Head == "" {
		return "", true // a new-root fork resumes as one: order was by slot time
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.ID == s.Head })
	if i < 0 {
		return "", false
	}
	if len(entries) > s.Entries && chainFrom(entries, i+1, s.Head) {
		return Head(entries), true
	}
	return s.Head, true
}

// chainFrom reports whether entries[i:] continue id in one parent chain.
func chainFrom(entries []Entry, i int, id string) bool {
	prev := id
	for _, e := range entries[i:] {
		if e.ParentID != prev {
			return false
		}
		prev = e.ID
	}
	return true
}

// removeSlots drops a transcript's whole writer-slot directory. Only Remove
// calls this: the transcript is gone, so every slot naming it is clutter.
func removeSlots(sessionPath string) {
	_ = os.RemoveAll(headsDir(sessionPath))
}
