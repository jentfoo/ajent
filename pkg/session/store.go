package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/strutil"
)

const (
	sessionDirPerm = 0o700
	maxSlugLen     = 40
)

// ErrNoSessions is returned when a workspace has no saved sessions.
var ErrNoSessions = errors.New("no sessions for this workspace")

// ErrNotFound is returned when an id matches nothing.
var ErrNotFound = errors.New("session not found")

// Store resolves and lists per-workspace session directories. Directories are
// <root>/<slug>-<hash> so they survive renames deterministically without an index.
type Store struct {
	root string
}

// NewStore returns the store rooted at <config dir>/sessions.
func NewStore() (*Store, error) {
	p, err := config.UserPath("sessions")
	if err != nil {
		return nil, err
	}
	return &Store{root: p}, nil
}

// StoreAt returns a store with an explicit root, for tests.
func StoreAt(root string) *Store { return &Store{root: root} }

// Dir resolves the directory holding one workspace's sessions, creating it.
func (s *Store) Dir(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	d := filepath.Join(s.root, slug(abs)+"-"+hash8(abs))
	if err := os.MkdirAll(d, sessionDirPerm); err != nil {
		return "", err
	}
	return d, nil
}

// Create starts a new session for workspace and returns its writer.
func (s *Store) Create(workspace string, d SessionData) (*Writer, error) {
	dir, err := s.Dir(workspace)
	if err != nil {
		return nil, err
	}
	name := time.Now().UTC().Format("2006-01-02T15-04-05Z") + "-" + NewID() + ".jsonl"
	return Create(filepath.Join(dir, name), d)
}

// List returns every session for workspace, newest first.
func (s *Store) List(workspace string) ([]Info, error) {
	dir, err := s.Dir(workspace)
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no sessions yet is not an error
		}
		return nil, err
	}
	var out []Info
	for _, f := range files {
		// side files (output-*.txt, editor-history.lines) are not transcripts; only
		// non-dir *.jsonl entries surface in the resume picker.
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, f.Name())
		if info, ok := readInfo(p); ok {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return b.Started.Compare(a.Started) })
	return out, nil
}

// Latest returns the most recent session for workspace.
func (s *Store) Latest(workspace string) (Info, error) {
	list, err := s.List(workspace)
	if err != nil {
		return Info{}, err
	}
	if len(list) == 0 {
		return Info{}, ErrNoSessions
	}
	return list[0], nil
}

// Find returns the session matching an exact id or a unique prefix.
func (s *Store) Find(workspace, id string) (Info, error) {
	list, err := s.List(workspace)
	if err != nil {
		return Info{}, err
	}
	matches := bulk.SliceFilter(func(in Info) bool {
		return in.ID == id || strings.HasPrefix(in.ID, id)
	}, list)
	switch len(matches) {
	case 0:
		return Info{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return Info{}, fmt.Errorf("ambiguous session id %q", id)
	}
}

// OutputPath returns a side file path for large streamed tool output, scoped to
// the session so it lives and dies with the transcript.
func (s *Store) OutputPath(sessionPath, callID string) string {
	return filepath.Join(filepath.Dir(sessionPath), "output-"+callID+".txt")
}

// Info describes one saved session for the resume picker.
type Info struct {
	Path     string
	ID       string
	Started  time.Time
	Model    string
	Messages int
	First    string // first user message, truncated
}

func readInfo(path string) (Info, bool) {
	entries, _, err := Read(path)
	if err != nil {
		return Info{}, false
	}
	var info Info
	info.Path = path
	// count only the persisted head's branch so message counts and the first user
	// prompt match what resuming would actually rebuild after a fork
	for _, e := range Branch(entries, headFor(path, entries)) {
		switch e.Type {
		case TypeSession:
			var sd SessionData
			_ = e.Decode(&sd)
			info.ID = e.ID
			info.Started = time.UnixMilli(e.TS).UTC()
			info.Model = sd.Model
		case TypeMessage:
			info.Messages++
		}
	}
	info.First = firstUserOn(Branch(entries, headFor(path, entries)))
	return info, true
}

// slug folds a workspace base name into a stable lowercase directory fragment.
func slug(p string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(p)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
	}
	return s
}

// hash8 returns the first 32 bits of sha256 over p, as hex.
func hash8(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:4])
}

const maxFirstLen = 80

// truncate caps s for Info.First display.
func truncate(s string) string {
	return strutil.Clip(s, maxFirstLen)
}
