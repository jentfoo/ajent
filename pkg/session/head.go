package session

import (
	"encoding/json"
	"errors"
	"os"
	"slices"

	"github.com/jentfoo/ajent/pkg/config"
)

// headSuffix names a transcript's cursor sidecar, the one mutable piece of an
// otherwise append-only design.
const headSuffix = ".head"

// headCursor is the active branch head of the transcript it sits beside.
// N is the entry count when written, to tell later appends from a rewind.
type headCursor struct {
	ID string `json:"id"`
	N  int    `json:"n,omitempty"`
}

// headPath returns the cursor sidecar beside the transcript at sessionPath.
func headPath(sessionPath string) string { return sessionPath + headSuffix }

// writeHead persists the branch head id and entry count for the transcript
// at sessionPath.
func writeHead(sessionPath, id string, n int) error {
	b, err := json.Marshal(headCursor{ID: id, N: n})
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(headPath(sessionPath), b, config.SecretPerm)
}

// removeHead drops one transcript's cursor. A missing sidecar is not an error.
func removeHead(sessionPath string) error {
	if err := os.Remove(headPath(sessionPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readHead returns the persisted branch cursor for the transcript at
// sessionPath. ok is false when it is missing, empty or corrupt.
func readHead(sessionPath string) (headCursor, bool) {
	b, err := os.ReadFile(headPath(sessionPath))
	if err != nil || len(b) == 0 {
		return headCursor{}, false
	}
	var cur headCursor
	if json.Unmarshal(b, &cur) != nil || cur.ID == "" {
		return headCursor{}, false
	}
	return cur, true
}

// headFor resolves the branch head for one transcript: the persisted cursor
// unless the file grew past the cursor's recorded count in one parent chain,
// which means unsynced appends from a crash rather than a fork. Falls back to
// tail recovery when the cursor is missing, corrupt or unresolvable.
func headFor(path string, entries []Entry) string {
	cur, ok := readHead(path)
	if !ok {
		return Head(entries)
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.ID == cur.ID })
	if i < 0 {
		return Head(entries)
	}
	if cur.N == 0 {
		return cur.ID // pre-count cursor, nothing to judge growth with
	}
	if len(entries) > cur.N && chainFrom(entries, cur.N, cur.ID) {
		return Head(entries)
	}
	return cur.ID
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
