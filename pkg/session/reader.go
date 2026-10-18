package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

// ErrNewerFormat marks a transcript stamped with a newer storage format: Read
// still returns its parsed entries (selection shows it Blocked), never appended.
var ErrNewerFormat = errors.New("session format is newer than this build supports")

// Read parses every complete line of a transcript into entries, tolerating
// garbage. A trailing partial write is skipped silently, while an unparseable middle
// line becomes a warning and never makes the session unopenable. Only a newer
// major format version is a hard error, returned together with the parsed
// entries so a blocked file stays identifiable.
func Read(path string) ([]Entry, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		out   []Entry
		warns []string
		r     = bufio.NewReader(f)
		n     int // line number for warnings
	)
	for {
		line, rerr := r.ReadString('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			// a malformed line yields an empty entry plus a warning, so the reader stays tolerant
			var e Entry
			warn := ""
			if err := json.Unmarshal([]byte(line[:len(line)-1]), &e); err != nil {
				warn = "unparseable entry: " + err.Error()
			}
			n++
			if e.ID != "" {
				out = append(out, e)
			} else if warn != "" {
				warns = append(warns, fmt.Sprintf("line %d: %s", n, warn))
			}
		} else if rerr == nil && len(line) > 0 {
			break // a final line with no newline is a partial write, skip silently
		}
		if rerr != nil {
			break // EOF or read error
		}
	}
	return out, warns, versionErr(out)
}

// versionErr returns a hard error when any session entry carries a format
// newer than this build understands.
func versionErr(entries []Entry) error {
	for _, e := range entries {
		if e.Type != TypeSession {
			continue
		}
		var sd SessionData
		if err := json.Unmarshal(e.Data, &sd); err == nil && sd.Version > sessionVersion {
			return fmt.Errorf("session format v%d is newer than supported v%d: %w", sd.Version, sessionVersion, ErrNewerFormat)
		}
	}
	return nil
}

// Branch returns the chain of entries from head back to its root, oldest first,
// stopping where a parent id is missing from entries or already visited. It is
// the only read path anything else uses, never raw file order.
func Branch(entries []Entry, head string) []Entry {
	if head == "" || len(entries) == 0 {
		return nil
	}
	idx := make(map[string]int, len(entries))
	for i := range entries {
		idx[entries[i].ID] = i
	}

	rev := make([]Entry, 0, len(entries)) // a linear chain is the common shape
	// Read accepts any well-formed line, so the parent chain can carry a cycle
	seen := make(map[string]bool, len(entries))
	id := head
	for id != "" {
		i, ok := idx[id]
		if !ok || seen[id] { // unknown or already-walked (cycle guard)
			break
		}
		seen[id] = true
		e := entries[i]
		rev = append(rev, e)
		id = e.ParentID
	}
	slices.Reverse(rev) // walked head..root; root..head order is the contract
	return rev
}

// Head returns the id of the last entry in file order.
func Head(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].ID
}
