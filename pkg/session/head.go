package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jentfoo/ajent/pkg/config"
)

// HeadCursor is the mutable pointer to where work continues after a fork: which
// transcript file in a directory and which entry id inside it. It is persisted as
// <session dir>/HEAD, the one mutable piece of an otherwise append-only design.
type HeadCursor struct {
	File string `json:"file"` // base name of one .jsonl session in the directory
	ID   string `json:"id"`   // active branch head inside that file
}

const headName = "HEAD"

func headPath(dir string) string { return filepath.Join(dir, headName) }

// WriteHead persists the active branch pointer for one session so resume
// continues from a fork rather than the transcript tail. It is overwritten on
// every SetHead and at turn boundaries.
func WriteHead(sessionPath, id string) error {
	cur := HeadCursor{File: filepath.Base(sessionPath), ID: id}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(headPath(filepath.Dir(sessionPath)), b, config.SecretPerm)
}

// ReadHead returns the persisted branch pointer for a session directory. ok is
// false when it is missing or corrupt; callers fall back to tail recovery.
func ReadHead(dir string) (HeadCursor, bool) {
	b, err := os.ReadFile(headPath(dir))
	if err != nil || len(b) == 0 {
		return HeadCursor{}, false
	}
	var cur HeadCursor
	if json.Unmarshal(b, &cur) != nil || cur.File == "" || cur.ID == "" {
		return HeadCursor{}, false
	}
	return cur, true
}

// headFor resolves the branch head for one transcript file: the persisted HEAD
// when it points into this file and its id exists, else tail recovery.
func headFor(path string, entries []Entry) string {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if cur, ok := ReadHead(dir); ok && cur.File == filepath.Base(path) {
			for _, e := range entries {
				if e.ID == cur.ID {
					return cur.ID
				}
			}
		}
	}
	return Head(entries)
}
