package refs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// entry is one path under the directory being completed. Directories keep
// completing with a trailing /; files are leaves.
type entry struct {
	path  string // absolute
	isDir bool
}

// Index completes @ references by listing only the single directory under the
// cursor, never walking the workspace tree: typing @ offers the cwd's immediate
// children and drilling through a trailing / re-lists one level at a time. A
// query is cheap (one ReadDir), so completion stays responsive however large or
// slow the filesystem; callers may run it off the UI lock.
type Index struct {
	root   string
	policy tools.PathPolicy // kept for parity with read/write/edit resolution
}

// NewIndex returns an index rooted at root. The root is where relative @ paths
// resolve, matching the keys read/write/edit use.
func NewIndex(root string, policy tools.PathPolicy) *Index {
	return &Index{root: root, policy: policy}
}

// Candidates returns paths matching query for an @ reference, ranked by (a)
// already in the conversation, (b) recent mtime, (c) fuzzy score. Directories
// complete with a trailing `/` so accepting one re-opens it deeper. Only the
// directory under the cursor is listed: an empty or partial path lists the
// root's children, and drilling through `dir/` descends one level per step. A
// `~`, `./` or absolute `/…` query keeps its leading form in each candidate so
// accepting inserts a usable path. VCS and dependency directories are skipped.
func (idx *Index) Candidates(query string, inConversation func(path string) bool) []tui.Completion {
	return idx.candidates(query, inConversation, true)
}

// ShellCandidates returns path candidates for query like Candidates, but also
// offers the VCS and dependency directories a `!` shell command may name.
func (idx *Index) ShellCandidates(query string) []tui.Completion {
	return idx.candidates(query, nil, false)
}

func (idx *Index) candidates(query string, inConversation func(path string) bool, skipVCS bool) []tui.Completion {
	if inConversation == nil {
		inConversation = func(string) bool { return false }
	}

	var qrel, base, displayPrefix string
	switch {
	case strings.HasPrefix(query, "~/") || query == "~":
		// ~ completes within the user's home directory.
		hdir := homeDir()
		if hdir == "" {
			return nil // no ~ completion without a resolvable home
		}
		qrel = strings.TrimPrefix(strings.TrimPrefix(query, "~"), "/")
		base = hdir
		displayPrefix = "~/"
	case filepath.IsAbs(query):
		qrel = strings.TrimPrefix(query, "/")
		base = string(filepath.Separator)
		displayPrefix = string(filepath.Separator)
	default:
		if d := "./"; strings.HasPrefix(query, d) {
			qrel = strings.TrimPrefix(query, d)
			displayPrefix = d
		} else {
			qrel = query // a bare workspace-relative path from the root
		}
		base = idx.root
	}

	target := dirTarget(base, qrel)
	entries, mtime := listDir(target, skipVCS)
	if len(entries) == 0 {
		return nil
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
		text := displayPrefix + fr
		if e.isDir {
			text += "/"
		}
		out = append(out, rankCandidate(text, inConversation(e.path), mtime[e.path], query))
	}
	slices.SortStableFunc(out, func(a, b tui.Completion) int { return b.Score - a.Score })
	return out
}

// dirTarget returns the directory whose immediate children should be offered for
// rel under base: the path itself when it ends in a separator (completing inside),
// else its parent. An empty rel lists base itself.
func dirTarget(base, rel string) string {
	if rel == "" {
		return base
	}
	p := filepath.Join(base, rel)
	if strings.HasSuffix(rel, "/") || strings.HasSuffix(rel, `\`) {
		return p // Join already cleaned the trailing separator
	}
	return filepath.Dir(p)
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

// listDir lists only path's immediate children: one directory deep, never a
// recursive walk. Every completion branch uses it so @ stays cheap however large
// the tree is. skipVCS drops VCS and dependency directories from the result.
func listDir(path string, skipVCS bool) ([]entry, map[string]time.Time) {
	des, err := os.ReadDir(path)
	if err != nil {
		return nil, make(map[string]time.Time)
	}
	entries := make([]entry, 0, len(des))
	mtimes := make(map[string]time.Time, len(des))
	for _, de := range des {
		p := filepath.Join(path, de.Name())
		if skipVCS && de.IsDir() && tools.IsSkippedDir(p) {
			continue
		}
		entries = append(entries, entry{path: p, isDir: de.IsDir()})
		mtimes[p] = statMod(p)
	}
	return entries, mtimes
}

// statMod returns a path's modification time, zero when it cannot be stated.
func statMod(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
