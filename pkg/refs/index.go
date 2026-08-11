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

	mu      sync.Mutex
	entries []entry // files and directories under root
	mtime   map[string]time.Time
	expires time.Time
}

// NewIndex returns an index rooted at root. The root is resolved through policy
// so completions match the same keys read/write/edit use.
func NewIndex(root string, policy tools.PathPolicy) *Index {
	return &Index{root: root, policy: policy, mtime: make(map[string]time.Time)}
}

// Candidates returns paths matching query, ranked by (a) already in the
// conversation, (b) recent mtime, (c) fuzzy score. Directories complete with a
// trailing `/` so accepting one re-opens it deeper. The result is relative to
// the root for display.
func (idx *Index) Candidates(query string, inConversation func(path string) bool) []tui.Completion {
	entries := idx.ensureFresh()
	if inConversation == nil {
		inConversation = func(string) bool { return false }
	}
	rel := relativeTo(idx.root, query)
	dir, prefix := filepath.Split(rel)

	var out []tui.Completion
	for _, e := range entries {
		fr, err := filepath.Rel(idx.root, e.path)
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
		if e.isDir {
			text += "/"
		}
		out = append(out, rankCandidate(text, inConversation(e.path), idx.mtime[e.path], query))
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
	if isGitRepo(root) {
		return enumerateRepo(root, &entries, mtimes), mtimes
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil // descend past the root but never offer it as a candidate
		}
		if isSkippedDir(p) && d.IsDir() {
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

// isGitRepo reports whether root is inside a git work tree.
func isGitRepo(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	return strings.TrimSpace(string(out)) == "true" && err == nil
}

// isSkippedDir reports whether path lies under a VCS or dependency directory.
func isSkippedDir(path string) bool {
	for part := range strings.SplitSeq(filepath.Clean(path), string(filepath.Separator)) {
		switch part {
		case ".git", ".hg", ".svn", "node_modules", ".venv":
			return true
		}
	}
	return false
}

// relativeTo returns path relative to root when inside it, joined for display.
func relativeTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
