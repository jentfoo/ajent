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
type headCursor struct {
	ID string `json:"id"`
}

// headPath returns the cursor sidecar beside the transcript at sessionPath.
func headPath(sessionPath string) string { return sessionPath + headSuffix }

// writeHead persists the active branch head for the transcript at sessionPath.
func writeHead(sessionPath, id string) error {
	b, err := json.Marshal(headCursor{ID: id})
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

// readHead returns the persisted branch head for the transcript at sessionPath.
// ok is false when it is missing, empty or corrupt.
func readHead(sessionPath string) (string, bool) {
	b, err := os.ReadFile(headPath(sessionPath))
	if err != nil || len(b) == 0 {
		return "", false
	}
	var cur headCursor
	if json.Unmarshal(b, &cur) != nil || cur.ID == "" {
		return "", false
	}
	return cur.ID, true
}

// headFor resolves the branch head for one transcript: its persisted cursor when
// the id still exists, else tail recovery.
func headFor(path string, entries []Entry) string {
	if id, ok := readHead(path); ok {
		if slices.ContainsFunc(entries, func(e Entry) bool { return e.ID == id }) {
			return id
		}
	}
	return Head(entries)
}
