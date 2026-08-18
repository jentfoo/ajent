package refs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// indexTTL bounds how long a cached enumeration is reused, since Complete may
// not block and a walk of a large tree is not free.
const indexTTL = 5 * time.Second

// entry is one path under the workspace root offered by completion. Directories
// keep completing with a trailing /; files are leaves.
type entry struct {
	path  string // absolute
	isDir bool
}

// Index enumerates workspace paths for @ completion, respecting .gitignore via
// git ls-files in a repo and a skip-listed walk otherwise. It caches the result
// on a TTL so Complete never blocks on a walk.
type Index struct {
	root   string
	policy tools.PathPolicy

	mu          sync.Mutex
	entries     []entry // files and directories under root
	mtime       map[string]time.Time
	expires     time.Time
	homeEntries []entry // lazily enumerated home dir for ~ completion
	homeMtime   map[string]time.Time
	homeExpires time.Time
}

// NewIndex returns an index rooted at root. The root is resolved through policy
// so completions match the same keys read/write/edit use.
func NewIndex(root string, policy tools.PathPolicy) *Index {
	return &Index{root: root, policy: policy, mtime: make(map[string]time.Time)}
}

// Candidates returns paths matching query, ranked by (a) already in the
// conversation, (b) recent mtime, (c) fuzzy score. Directories complete with a
// trailing `/` so accepting one re-opens it deeper. The result is relative to
// the root for display; a `~` or `~/…` query completes within the user's home
// directory instead, keeping the leading ~ in each candidate.
func (idx *Index) Candidates(query string, inConversation func(path string) bool) []tui.Completion {
	if inConversation == nil {
		inConversation = func(string) bool { return false }
	}

	// a ~ or ~/… query completes within the user's home directory.
	home := strings.HasPrefix(query, "~/") || query == "~"
	base := idx.root // base directory candidates are displayed relative to
	var entries []entry
	var mtime map[string]time.Time
	if home {
		hdir := homeDir()
		if hdir == "" {
			return nil // no ~ completion without a resolvable home
		}
		entries, mtime = idx.ensureHomeFresh(hdir)
		base = hdir
	} else {
		entries = idx.ensureFresh()
		mtime = idx.mtime
	}
	if len(entries) == 0 {
		return nil
	}

	// the query relative to base: strip a leading ~ (and slash) for home queries,
	// keep as-is for workspace ones.
	qrel := query
	if home {
		qrel = strings.TrimPrefix(strings.TrimPrefix(query, "~"), "/")
	}
	dir, prefix := filepath.Split(qrel)

	var out []tui.Completion
	for _, e := range entries {
		fr, err := filepath.Rel(base, e.path)
		if err != nil || !strings.HasPrefix(fr, dir) {
			continue
		}
		name := strings.TrimPrefix(fr, dir)
		// offer immediate children only; deeper paths are reached by drilling
		if name == "" || strings.ContainsRune(name, filepath.Separator) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		text := fr
		if home {
			text = "~/" + text // keep the ~ so accepting inserts a usable path
		}
		if e.isDir {
			text += "/"
		}
		out = append(out, rankCandidate(text, inConversation(e.path), mtime[e.path], query))
	}
	slices.SortStableFunc(out, func(a, b tui.Completion) int { return b.Score - a.Score })
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// rankCandidate builds one completion, scoring by conversation presence first,
// then recency, then fuzzy match.
func rankCandidate(text string, inConvo bool, mt time.Time, query string) tui.Completion {
	score := 0
	if inConvo {
		score += 1000
	}
	score += recentScore(mt)
	if q, ok := tui.MatchScore(text, query); ok {
		score += q
	}
	return tui.Completion{Text: text, Label: text, Score: score}
}

// recentScore gives newer files a higher score on a 0..300 scale.
func recentScore(mt time.Time) int {
	if mt.IsZero() {
		return 0
	}
	days := time.Since(mt).Hours() / 24
	if days < 0 {
		days = 0
	}
	if days > 30 {
		return 0
	}
	return int(300 * (1 - days/30))
}

var userHome = os.UserHomeDir // injectable for tests

// homeDir returns the user's home directory, or "" when it cannot be resolved.
func homeDir() string {
	home, err := userHome()
	if err != nil || home == "" {
		return ""
	}
	return home
}

// ensureHomeFresh refreshes the cached enumeration of root (the user's home)
// when its TTL has elapsed.
func (idx *Index) ensureHomeFresh(root string) ([]entry, map[string]time.Time) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if time.Now().Before(idx.homeExpires) && idx.homeEntries != nil {
		return idx.homeEntries, idx.homeMtime
	}
	entries, mtimes := enumerate(root)
	idx.homeEntries = entries
	idx.homeMtime = mtimes
	idx.homeExpires = time.Now().Add(indexTTL)
	return entries, mtimes
}

// ensureFresh refreshes the cached entry list when the TTL has elapsed.
func (idx *Index) ensureFresh() []entry {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if time.Now().Before(idx.expires) && idx.entries != nil {
		return idx.entries
	}
	entries, mtimes := enumerate(idx.root)
	idx.entries = entries
	idx.mtime = mtimes
	idx.expires = time.Now().Add(indexTTL)
	return entries
}

// enumerate lists files and directories under root, respecting .gitignore in a
// git repo and skipping VCS/dependency dirs otherwise.
func enumerate(root string) ([]entry, map[string]time.Time) {
	var entries []entry
	mtimes := make(map[string]time.Time)
	if tools.IsGitRepo(root) {
		return enumerateRepo(root, &entries, mtimes), mtimes
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil // descend past the root but never offer it as a candidate
		}
		if tools.IsSkippedDir(p) && d.IsDir() {
			return filepath.SkipDir
		}
		entries = append(entries, entry{path: p, isDir: d.IsDir()})
		mtimes[p] = statMod(p)
		return nil
	})
	return entries, mtimes
}

// enumerateRepo derives directory entries from git's tracked/untracked file
// list: every ancestor of a listed file up to the root is offered as a dir.
func enumerateRepo(root string, entries *[]entry, mtimes map[string]time.Time) []entry {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// -z: NUL-delimited and never quotes paths (spaces/unicode stay literal)
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-coz", "--exclude-standard")
	out, _ := cmd.Output()

	rootClean := filepath.Clean(root)
	seen := map[string]struct{}{}
	addPath := func(abs string, isDir bool) {
		if _, ok := seen[abs]; !ok {
			seen[abs] = struct{}{}
			*entries = append(*entries, entry{path: abs, isDir: isDir})
			mtimes[abs] = statMod(abs)
		}
	}

	repoFiles := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for _, relline := range repoFiles {
		if relline == "" {
			continue
		}
		abs := filepath.Join(root, relline)
		addPath(abs, false)

		// every ancestor below the workspace root completes as a directory.
		dir := filepath.Clean(filepath.Dir(abs))
		for dir != rootClean {
			addPath(dir, true)
			parent := filepath.Dir(dir)
			if parent == dir {
				break // filesystem root safety; the workspace root should be hit first
			}
			dir = parent
		}
	}
	return *entries
}

// statMod returns a path's modification time, zero when it cannot be stated.
func statMod(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
