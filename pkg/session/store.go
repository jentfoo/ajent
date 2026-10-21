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
	"github.com/jentfoo/ajent/pkg/tools"
)

const (
	sessionDirPerm = 0o700
	maxSlugLen     = 64
)

var userHome = os.UserHomeDir // injectable for tests

// ErrNoSessions is returned when a workspace has no saved sessions.
var ErrNoSessions = errors.New("no sessions for this workspace")

// ErrNotFound is Find's not-found sentinel: its concrete missError results
// match it through errors.Is while carrying the target-specific wording.
var ErrNotFound = errors.New("session not found")

// missError carries Find's miss wording, including any damaged-transcript hint,
// so a miss never reads as an empty store.
type missError struct {
	detail string
}

func (e *missError) Error() string { return e.detail }

func (e *missError) Unwrap() error { return ErrNotFound }

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
	name := slug(abs, home)
	dir := name + "-" + hash4(abs)
	if name == "" {
		dir = hash4(abs) // the home directory itself has nothing to encode
	}
	d := filepath.Join(s.root, dir)
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

// List returns every session for workspace, most recently used first. A
// transcript whose format this build cannot read still appears,
// marked Info.Blocked; callers surface those as they see fit and must never
// pass one to Open. One holding entries yet no decodable session identity is
// the reverse: it never lists (Damaged), and Find reaches it only by path.
func (s *Store) List(workspace string) ([]Info, error) {
	infos, err := s.scanInfos(workspace)
	if err != nil {
		return nil, err
	}
	// identified rows carry Path; damaged ones do too, but never list as sessions
	out := bulk.SliceFilterInPlace(func(in Info) bool { return in.Path != "" && !in.Damaged }, infos)
	// ordered on last use, with start time only as a tie break. Stable so equal
	// clocks keep the name order above rather than the ReadDir arrival order.
	slices.SortStableFunc(out, func(a, b Info) int {
		return cmp.Or(b.Updated.Compare(a.Updated), b.Started.Compare(a.Started))
	})
	return out, nil
}

// transcriptPaths returns workspace's .jsonl files in name order: the scan order
// every directory walk here keeps so results land deterministic.
func (s *Store) transcriptPaths(workspace string) ([]string, error) {
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
	names := make([]string, 0, len(files)) // jsonl transcripts are a share of dir entries
	for _, f := range files {
		// side files (output-*.txt, editor-history.lines) are not transcripts, and only
		// non-dir *.jsonl entries surface in the resume picker.
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			names = append(names, f.Name())
		}
	}
	slices.Sort(names) // parse in name order so results land deterministic
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(dir, name)
	}
	return paths, nil
}

// Find returns the session matching target: an exact name first (case
// insensitive), then a full id or unique prefix, then as a filesystem path onto
// workspace's session directory. A match blocked behind a newer format resolves
// too, with Info.Blocked set; one whose identity cannot decode lists nowhere but
// resolves by path with Info.Damaged set, keeping --delete able to remove it.
func (s *Store) Find(workspace, target string) (Info, error) {
	info, found, err := s.findListed(workspace, target)
	if err != nil || found {
		return info, err
	}
	// no row answered: the file itself is the last handle. A transcript whose
	// identity cannot decode never lists, so without this its only way out of the
	// directory would be rm.
	if byPath, ok := s.findByPath(workspace, target); ok {
		return byPath, nil
	}
	detail := fmt.Sprintf("no session matches %q", target)
	if damaged, derr := s.Damaged(workspace); derr == nil { // best effort hint
		if note := DescribeDamaged(damaged); note != "" {
			detail += "; " + note
		}
	}
	return Info{}, &missError{detail: detail}
}

// findListed resolves target against the listing alone: an exact name first
// (case insensitive), then a full id or unique prefix. found is false when no
// listed row answers to target. It never touches files by path, so the name
// surfaces (FindNamed, NameConflict) cannot reach a transcript that way.
func (s *Store) findListed(workspace, target string) (Info, bool, error) {
	list, err := s.List(workspace)
	if err != nil {
		return Info{}, false, err
	}
	// a name is typed deliberately, so an exact hit wins over any id prefix it
	// happens to share characters with.
	named := bulk.SliceFilter(func(in Info) bool {
		return in.Name != "" && strings.EqualFold(in.Name, target)
	}, list)
	if len(named) > 0 {
		if len(named) > 1 {
			return Info{}, false, fmt.Errorf("ambiguous session name %q", target)
		}
		return named[0], true, nil
	}

	matches := bulk.SliceFilter(func(in Info) bool {
		return in.ID == target || strings.HasPrefix(in.ID, target)
	}, list)
	switch len(matches) {
	case 1:
		return matches[0], true, nil
	case 0:
		return Info{}, false, nil
	default:
		return Info{}, false, fmt.Errorf("ambiguous session id %q", target)
	}
}

// findByPath resolves target as a transcript file inside workspace's session
// directory. ok is false when the target names no readable .jsonl file there;
// ids and names never carry that suffix, so they cannot reach the filesystem.
func (s *Store) findByPath(workspace, target string) (Info, bool) {
	if !strings.HasSuffix(target, ".jsonl") { // ids and names never carry the suffix
		return Info{}, false
	}
	dir, err := s.Dir(workspace)
	if err != nil {
		return Info{}, false
	}
	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p) // a bare basename resolves inside the session dir too
	}
	if rel, ok := tools.WithinRoot(dir, p); !ok || rel == "." {
		return Info{}, false // the directory itself, or something outside it
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return Info{}, false
	}
	info, ok := readInfo(p)
	return info, ok // a damaged classification is exactly what this resolves
}

// FindNamed resolves a name the way --resume does, returning whether it exists.
// It errors when the target reaches a session by id rather than by that name,
// so a name typed with intent is never silently matched to an unrelated id.
func (s *Store) FindNamed(workspace, target string) (Info, bool, error) {
	info, found, err := s.findListed(workspace, target)
	if err != nil || !found {
		return Info{}, false, err
	} else if !strings.EqualFold(info.Name, target) {
		return Info{}, false, fmt.Errorf("session name %q matches session id %s", target, info.ID)
	}
	return info, true, nil
}

// NameConflict reports why name cannot identify the session at selfPath: it is
// usable when it reaches nothing, or reaches that same session. Pass an empty
// selfPath for a session that does not exist yet.
func (s *Store) NameConflict(workspace, name, selfPath string) error {
	info, found, err := s.findListed(workspace, name)
	if err != nil {
		return err // ambiguous, or the directory could not be read
	} else if !found || info.Path == selfPath {
		return nil
	} else if info.Name != "" {
		return fmt.Errorf("%w: %q names another session", ErrNameConflict, name)
	}
	return fmt.Errorf("%w: %q matches session id %s", ErrNameConflict, name, info.ID)
}

// Stale returns the workspace's unnamed sessions last used before cutoff, most recently used first.
// A named session is never stale: the name is what --session resumes it by. Nor
// is a newer-format one swept: this build cannot read what it would be deleting.
// Damaged transcripts never sweep either: they never list, so only a deliberate
// path-targeted --delete removes them.
func (s *Store) Stale(workspace string, cutoff time.Time) ([]Info, error) {
	list, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	// List already orders on last use, the field the sweep selects by
	return bulk.SliceFilterInPlace(func(in Info) bool {
		return !in.Blocked && in.Name == "" && in.Updated.Before(cutoff)
	}, list), nil
}

// scanInfos classifies every transcript in workspace's directory in name order;
// files holding nothing this build could ever classify come back as zero Info.
func (s *Store) scanInfos(workspace string) ([]Info, error) {
	paths, err := s.transcriptPaths(workspace)
	if err != nil {
		return nil, err
	}
	return scanEach(paths, func(p string) Info {
		info, _ := readInfo(p)
		return info
	}), nil
}

// Damaged returns every transcript in workspace's directory that holds entries
// yet no decodable session identity, in name order. These never list as
// sessions; Find resolves one only by its path.
func (s *Store) Damaged(workspace string) ([]Info, error) {
	infos, err := s.scanInfos(workspace)
	if err != nil {
		return nil, err
	}
	return bulk.SliceFilterInPlace(func(in Info) bool { return in.Damaged }, infos), nil
}

// Remove deletes one saved transcript with its sidecars: the branch cursor and
// any torn-tail quarantine file. Other sessions in the directory (and editor
// history) are untouched. Sidecar cleanup is best effort: an error means the
// transcript itself survived.
func (s *Store) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = removeHead(path) // a session whose transcript is gone must not report as kept
	_ = os.Remove(tornPath(path))
	return nil
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

	// Blocked marks a transcript whose storage format this build cannot read
	// identity fields are filled, branch-derived ones (Messages, First) are not,
	// and Open must never see it.
	Blocked bool

	// Damaged marks a transcript holding entries yet no decodable session
	// identity: identity fields stay empty and listing drops the row. Find
	// resolves one only by its path, so --delete can still remove it; Open must
	// never see it either.
	Damaged bool
}

// Label returns what --resume takes back for a saved session: its name, or its
// id when unnamed.
func (i Info) Label() string {
	if i.Name != "" {
		return i.Name
	}
	return i.ID
}

func readInfo(path string) (Info, bool) {
	entries, _, err := Read(path)
	if err != nil {
		return blockedInfo(path, entries, err)
	}
	var info Info
	// identity belongs to the file, not its active branch: a head sitting on a
	// second root never reaches line 1's session entry, yet still names the file.
	info.ID, info.Started, info.Model = sessionIdentity(entries)
	if info.ID == "" {
		if len(entries) == 0 {
			// no entries at all: a crash before Create's first write landed, or pure
			// garbage. Not a session, and nothing this build could ever classify.
			return Info{}, false
		}
		// entries without identity hold real work this build cannot classify. The
		// row never lists, but it stays reachable by path so the damage can surface
		// and --delete can remove it deliberately rather than via raw rm.
		info.Path = path
		info.Damaged = true
		info.Name = NameOf(entries)
		info.Updated = lastUsed(path, entries)
		return info, true
	}
	info.Path = path
	branch := Branch(entries, headFor(path, entries))
	for _, e := range branch {
		switch e.Type {
		case TypeMessage:
			info.Messages++
		}
	}
	info.Name = NameOf(entries)
	info.Updated = lastUsed(path, entries)
	info.First = firstUserOn(branch)
	return info, true
}

// blockedInfo classifies a failed read: only a newer format arrives with its
// entries parsed, and those still identify the file so List and Find can see
// it. Branch-derived fields stay unset because this build must not interpret
// what it cannot open.
func blockedInfo(path string, entries []Entry, err error) (Info, bool) {
	if !errors.Is(err, ErrNewerFormat) || len(entries) == 0 {
		return Info{}, false // missing or unreadable is simply not a session
	}
	var info Info
	info.Path = path
	info.Blocked = true
	info.ID, info.Started, info.Model = sessionIdentity(entries)
	info.Name = NameOf(entries)
	info.Updated = lastUsed(path, entries)
	return info, true
}

// DescribeDamaged renders the hint naming damaged transcripts: what they are and
// the path handle that reaches them. Empty for no input.
func DescribeDamaged(infos []Info) string {
	if len(infos) == 0 {
		return ""
	}
	if len(infos) == 1 {
		return fmt.Sprintf("damaged transcript %s holds entries this build cannot classify (delete it by full path)", infos[0].Path)
	}
	paths := make([]string, len(infos))
	for i, in := range infos {
		paths[i] = in.Path
	}
	return fmt.Sprintf("%s hold entries this build cannot classify (delete one by full path): %s",
		strutil.Plural(len(infos), "damaged transcript"), strings.Join(paths, ", "))
}

// sessionIdentity returns the transcript's own id, start time and model from raw
// file order: these identify the file as a whole, like NameOf.
func sessionIdentity(entries []Entry) (id string, started time.Time, model string) {
	for _, e := range entries {
		if e.Type != TypeSession {
			continue
		}
		var sd SessionData
		if err := e.Decode(&sd); err == nil {
			return e.ID, time.UnixMilli(e.TS).UTC(), sd.Model
		}
	}
	return "", time.Time{}, ""
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

// slug encodes abs as a flat lowercase name: path segments joined with "_", with anything
// outside [a-z0-9] dropped. A path outside home keeps its leading separator as a "_" prefix.
// An over-long name keeps its tail, where the project name is.
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
	if rel, ok := tools.WithinRoot(home, abs); ok {
		return rel, true
	}
	return abs, false
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

const maxFirstLen = 80 // display cap shared by Info.First and the picker labels

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
		if text := llm.FirstBlockText(md.Message.Content); text != "" {
			return strutil.Clip(strings.TrimSpace(text), maxFirstLen)
		}
	}
	return ""
}
