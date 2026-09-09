package session

import (
	"cmp"
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
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

const (
	sessionDirPerm = 0o700
	maxSlugLen     = 64
)

var userHome = os.UserHomeDir // injectable for tests

// ErrNoSessions is returned when a workspace has no saved sessions.
var ErrNoSessions = errors.New("no sessions for this workspace")

// ErrNotFound is returned when an id matches nothing.
var ErrNotFound = errors.New("session not found")

// ErrNameConflict is returned when a name already reaches another session.
var ErrNameConflict = errors.New("session name already in use")

// Store resolves and lists per-workspace session directories. Directories are
// <root>/<slug>-<hash>, the slug encoding the whole workspace path so the
// directory is recognisable, the hash pinning it so a rename never collides.
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
	home, herr := userHome()
	if herr != nil {
		home = "" // no home to anchor against, so the path encodes absolute
	}
	d := filepath.Join(s.root, dirName(abs, home))
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

// List returns every session for workspace, most recently used first.
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
	// ordered on last use, with start time only as a tie break
	slices.SortFunc(out, func(a, b Info) int {
		return cmp.Or(b.Updated.Compare(a.Updated), b.Started.Compare(a.Started))
	})
	return out, nil
}

// Latest returns the most recently used session for workspace.
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

// Find returns the session matching target: an exact name first (case
// insensitive), then an exact id, then a unique id prefix.
func (s *Store) Find(workspace, target string) (Info, error) {
	list, err := s.List(workspace)
	if err != nil {
		return Info{}, err
	}
	// a name is typed deliberately, so an exact hit wins over any id prefix it
	// happens to share characters with.
	named := bulk.SliceFilter(func(in Info) bool {
		return in.Name != "" && strings.EqualFold(in.Name, target)
	}, list)
	if len(named) > 0 {
		if len(named) > 1 {
			return Info{}, fmt.Errorf("ambiguous session name %q", target)
		}
		return named[0], nil
	}

	matches := bulk.SliceFilter(func(in Info) bool {
		return in.ID == target || strings.HasPrefix(in.ID, target)
	}, list)
	switch len(matches) {
	case 0:
		return Info{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return Info{}, fmt.Errorf("ambiguous session id %q", target)
	}
}

// NameConflict reports why name cannot identify the session at selfPath: it is
// usable when it reaches nothing, or reaches that same session. Pass an empty
// selfPath for a session that does not exist yet.
func (s *Store) NameConflict(workspace, name, selfPath string) error {
	info, err := s.Find(workspace, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err // ambiguous, or the directory could not be read
	} else if info.Path == selfPath {
		return nil
	} else if info.Name != "" {
		return fmt.Errorf("%w: %q names another session", ErrNameConflict, name)
	}
	return fmt.Errorf("%w: %q matches session id %s", ErrNameConflict, name, info.ID)
}

// Stale returns the workspace's unnamed sessions last used before cutoff, most
// recently used first. A named session is never stale: the name is what --session
// resumes it by.
func (s *Store) Stale(workspace string, cutoff time.Time) ([]Info, error) {
	list, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	// List already orders on last use, the field the sweep selects by
	return bulk.SliceFilterInPlace(func(in Info) bool {
		return in.Name == "" && in.Updated.Before(cutoff)
	}, list), nil
}

// Remove deletes one saved transcript and its branch cursor. Other sessions in
// the directory (and editor history) are untouched.
func (s *Store) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return removeHead(path)
}

// Info describes one saved session for the resume picker.
type Info struct {
	Path     string
	ID       string
	Name     string // optional human-readable id, empty when unnamed
	Started  time.Time
	Updated  time.Time // last recorded activity: newest entry, or the file mtime when later
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
	info.Name = NameOf(entries)
	info.Updated = lastUsed(path, entries)
	info.First = firstUserOn(Branch(entries, headFor(path, entries)))
	return info, true
}

// lastUsed returns when the transcript was last written: the newest entry, or the
// file mtime when that is later.
func lastUsed(path string, entries []Entry) time.Time {
	var out time.Time
	// raw file order, not the branch: like NameOf this describes the file, and work
	// left on an abandoned fork was still work.
	if len(entries) > 0 {
		newest := slices.MaxFunc(entries, func(a, b Entry) int { return cmp.Compare(a.TS, b.TS) })
		if newest.TS > 0 {
			out = time.UnixMilli(newest.TS).UTC()
		}
	}
	// the later of the two wins, so an out of band write or a restored backup is
	// never mistaken for an abandoned session.
	if fi, err := os.Stat(path); err == nil {
		if mt := fi.ModTime().UTC(); mt.After(out) {
			out = mt
		}
	}
	return out
}

// maxNameLen caps a session name so it stays a handle rather than a title.
const maxNameLen = 64

// NameOf returns the session's name, empty when it has none.
func NameOf(entries []Entry) string {
	// the name identifies the transcript file, not a branch, so this reads raw
	// file order: a rename left on an abandoned fork must not un-name the session
	// while the entry is still on disk.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Type != TypeSessionName {
			continue
		}
		var nd NameData
		if err := e.Decode(&nd); err == nil && nd.Name != "" {
			return nd.Name
		}
	}
	for _, e := range entries {
		if e.Type != TypeSession {
			continue
		}
		var sd SessionData
		if err := e.Decode(&sd); err == nil {
			return sd.Name
		}
	}
	return ""
}

// ValidateName returns the canonical form of a session name, or an error when it
// is empty, longer than maxNameLen, starts with a dash, or holds anything outside
// letters, digits, dash, underscore, dot and slash.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errors.New("session name cannot be empty")
	case len(name) > maxNameLen:
		return "", fmt.Errorf("session name is longer than %d characters", maxNameLen)
	case strings.HasPrefix(name, "-"):
		// a leading dash would be read as a flag on the way back in
		return "", errors.New("session name cannot start with '-'")
	}
	for _, r := range name {
		if !nameRune(r) {
			return "", fmt.Errorf("session name cannot contain %q", r)
		}
	}
	return name, nil
}

// nameRune reports whether r may appear in a session name. The set is limited to
// characters a shell leaves literal, so the printed resume command needs no
// quoting and a pasted name can never run something else.
func nameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
		r == '-' || r == '_' || r == '.' || r == '/'
}

// dirName returns the session directory name for the absolute workspace path
// abs, anchored on the user home directory home (empty when there is none).
func dirName(abs, home string) string {
	s := slug(abs, home)
	if s == "" {
		return hash4(abs) // the home directory itself has nothing to encode
	}
	return s + "-" + hash4(abs)
}

// slug encodes abs as a flat lowercase name: path segments joined with "_",
// with anything outside [a-z0-9] dropped. A path outside home keeps its leading
// separator as a "_" prefix. An over-long name keeps its tail, where the
// project name is.
func slug(abs, home string) string {
	rel, under := relHome(abs, home)
	var b strings.Builder
	if !under {
		b.WriteByte('_') // the leading separator of an absolute path
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		clean := keepAlnum(seg)
		if clean == "" {
			continue
		}
		if b.Len() > 0 && b.String()[b.Len()-1] != '_' {
			b.WriteByte('_')
		}
		b.WriteString(clean)
	}
	s := b.String()
	if len(s) > maxSlugLen {
		s = strings.TrimLeft(s[len(s)-maxSlugLen:], "_")
	}
	return s
}

// relHome returns abs relative to home, and whether it is under it.
func relHome(abs, home string) (string, bool) {
	if home == "" {
		return abs, false
	}
	rel, err := filepath.Rel(home, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs, false
	}
	return rel, true
}

// keepAlnum lowercases s and drops everything outside [a-z0-9].
func keepAlnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hash4 returns the first 16 bits of sha256 over p, as hex.
func hash4(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:2])
}

const maxFirstLen = 80

// truncate caps s for Info.First display.
func truncate(s string) string {
	return strutil.Clip(s, maxFirstLen)
}

// firstUserOn returns the first user text on a branch, truncated for display.
func firstUserOn(branch []Entry) string {
	for _, e := range branch {
		if e.Type != TypeMessage {
			continue
		}
		var md MessageData
		if err := e.Decode(&md); err != nil || md.Message.Role != llm.RoleUser {
			continue
		}
		for _, b := range md.Message.Content {
			if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
				return truncate(strings.TrimSpace(tb.Text))
			}
		}
	}
	return ""
}
