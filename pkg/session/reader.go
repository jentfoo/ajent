package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Read parses every complete line of a transcript into entries, tolerating
// garbage. A trailing partial write is skipped silently; an unparseable middle
// line becomes a warning and never makes the session unopenable. Only a newer
// major format version is a hard error.
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
			e, warn := parseLine([]byte(line[:len(line)-1]))
			n++
			if e.ID != "" {
				out = append(out, e)
			} else if warn != "" {
				warns = append(warns, fmt.Sprintf("line %d: %s", n, warn))
			}
		} else if rerr == nil && len(line) > 0 {
			break // a final line with no newline is a partial write; skip silently
		}
		if rerr != nil {
			break // EOF or read error
		}
	}
	return out, warns, versionErr(out)
}

// parseLine decodes one entry. A malformed line yields an empty entry plus a
// warning so the reader stays tolerant.
func parseLine(line []byte) (Entry, string) {
	var e Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return Entry{}, "unparseable entry: " + err.Error()
	}
	return e, ""
}

// versionErr returns a hard error when any session entry carries a format newer
// than this build understands.
func versionErr(entries []Entry) error {
	for _, e := range entries {
		if e.Type != TypeSession {
			continue
		}
		var sd SessionData
		if err := json.Unmarshal(e.Data, &sd); err == nil && sd.Version > sessionVersion {
			return fmt.Errorf("session format v%d is newer than supported v%d", sd.Version, sessionVersion)
		}
	}
	return nil
}

// Branch returns the chain from head back to its root by walking parentID
// through an id index. It is the only read path anything else uses — never raw
// file order.
func Branch(entries []Entry, head string) []Entry {
	if head == "" || len(entries) == 0 {
		return nil
	}
	idx := make(map[string]int, len(entries))
	for i := range entries {
		idx[entries[i].ID] = i
	}

	var rev []Entry
	id := head
	for steps := 0; id != "" && steps <= len(entries); steps++ {
		i, ok := idx[id]
		if !ok {
			break // unknown or already-visited id stops the walk
		}
		e := entries[i]
		rev = append(rev, e)
		id = e.ParentID
	}
	out := make([]Entry, len(rev))
	for i, e := range rev { // reverse to root..head order
		out[len(rev)-1-i] = e
	}
	return out
}

// Head returns the id of the last entry in file order.
func Head(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].ID
}
