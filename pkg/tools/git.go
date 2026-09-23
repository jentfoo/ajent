package tools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// errNotGit reports that path is not inside a git work tree.
var errNotGit = errors.New("not a git repository")

// errNoCommits reports an unborn HEAD: the repository exists but is empty.
var errNoCommits = errors.New("repository has no commits yet")

// git_* engine: go-git, in process only. No subprocess, never exec of the git
// binary, so filters, hooks and pagers in a hostile work tree cannot run.

// openGitRepo opens the repository containing path.
func openGitRepo(path string) (*git.Repository, error) {
	r, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: true})
	if errors.Is(err, git.ErrRepositoryNotExists) {
		r, err = git.PlainOpen(path) // bare repositories have no .git to detect
	}
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, errNotGit
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// gitWorktree returns the work tree of r, naming bare repositories clearly.
func gitWorktree(r *git.Repository) (*git.Worktree, error) {
	w, err := r.Worktree()
	if errors.Is(err, git.ErrIsBareRepository) {
		return nil, errors.New("bare repository has no working tree")
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// gitCommit resolves rev inside r to a commit. An empty rev means HEAD.
func gitCommit(r *git.Repository, rev string) (*object.Commit, error) {
	if rev == "" {
		rev = "HEAD"
	}
	hash, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			if rev == "HEAD" {
				return nil, errNoCommits
			}
			return nil, fmt.Errorf("unknown revision %q", rev)
		}
		return nil, err
	}
	return r.CommitObject(*hash)
}

// gitTree returns the tree of c's snapshot, or the empty tree before any commit.
func gitTree(c *object.Commit) (*object.Tree, error) {
	if c == nil {
		return &object.Tree{}, nil
	}
	return c.Tree()
}

// gitSide is one snapshot of a path: blob content plus repo metadata. nil is
// the /dev/null side of a creation or deletion.
type gitSide struct {
	path string
	hash plumbing.Hash // zero for the work-tree side, where the hash is not repo-canonical
	mode filemode.FileMode
	data string
	bin  bool
}

// gitBlobSide reads path out of a tree. A missing entry or submodule yields
// nil with no error; a read failure returns the error, so a broken object is
// never misread as a deletion.
func gitBlobSide(t *object.Tree, path string) (*gitSide, error) {
	if t == nil {
		return nil, nil
	}
	f, err := t.File(path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if f.Mode == filemode.Submodule {
		return nil, nil
	}
	data, err := f.Contents()
	if err != nil {
		return nil, err
	}
	return &gitSide{path: path, hash: f.Hash, mode: f.Mode, data: data, bin: binary([]byte(data))}, nil
}

// gitDiskSide reads one path off the work tree. dir reports a directory at
// path, most plausibly an unopened submodule checkout. A missing or unreadable
// file yields a nil side, the /dev/null side of a deletion.
func gitDiskSide(root, path string) (side *gitSide, dir bool) {
	full := filepath.Join(root, filepath.FromSlash(path))
	fi, err := os.Lstat(full)
	if err != nil {
		return nil, false
	}
	if fi.IsDir() {
		return nil, true
	}
	var data string
	if fi.Mode()&os.ModeSymlink != 0 { // symlink blobs hold the target path
		data, err = os.Readlink(full)
	} else {
		var raw []byte
		raw, err = os.ReadFile(full)
		data = string(raw)
	}
	if err != nil {
		return nil, false
	}
	mode, err := filemode.NewFromOSFileMode(fi.Mode())
	if err != nil {
		return nil, false
	}
	return &gitSide{path: path, mode: mode, data: data, bin: binary([]byte(data))}, false
}

// gitPathScope matches an exact path or anything beneath it as a directory.
// An empty scope matches everything.
func gitPathScope(file string) func(string) bool {
	if file == "" {
		return func(string) bool { return true }
	}
	return func(path string) bool {
		return path == file || strings.HasPrefix(path, file+"/")
	}
}

// gitDiffTreeText renders the rename-aware unified patch between two trees as
// git-shaped per-file blocks, limited to file's path scope when non-empty.
// Notes name omitted content: submodules.
func gitDiffTreeText(ctx context.Context, from, to *object.Tree, file string) (string, []string, error) {
	keep := gitPathScope(file) // matches everything when file is empty
	changes, err := object.DiffTreeWithOptions(ctx, from, to, object.DefaultDiffTreeOptions)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var notes []string
	for _, ch := range changes {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if !keep(ch.From.Name) && !keep(ch.To.Name) {
			continue
		}
		fromFile, toFile, err := ch.Files()
		if err != nil {
			return "", nil, err
		}
		if (fromFile != nil && fromFile.Mode == filemode.Submodule) ||
			(toFile != nil && toFile.Mode == filemode.Submodule) {
			notes = append(notes, "submodule "+changePath(ch)+" changed")
			continue
		}
		from, err := fileSide(fromFile)
		if err != nil {
			return "", nil, err
		}
		to, err := fileSide(toFile)
		if err != nil {
			return "", nil, err
		}
		writeGitFileDiff(&b, from, to)
	}
	return strings.TrimRight(b.String(), "\n"), notes, nil
}

// gitDiffWorktreeText renders committed state at base against the files on
// disk, like `git diff <ref>`: every difference since base, committed or not,
// limited to file's path scope when non-empty. Notes name untracked files,
// which git does not diff either.
func gitDiffWorktreeText(ctx context.Context, r *git.Repository, base *object.Tree, file string) (string, []string, error) {
	w, err := gitWorktree(r)
	if err != nil {
		return "", nil, err
	}
	st, err := w.StatusWithOptions(git.StatusOptions{Strategy: git.Preload})
	if err != nil {
		return "", nil, err
	}
	root := w.Filesystem.Root()

	keep := gitPathScope(file) // matches everything when file is empty
	paths := make(map[string]struct{}, len(st))
	_ = base.Files().ForEach(func(f *object.File) error {
		if keep(f.Name) {
			paths[f.Name] = struct{}{}
		}
		return nil
	})
	for path := range st {
		if keep(path) {
			paths[path] = struct{}{}
		}
	}

	var b strings.Builder
	var notes []string
	var untracked int
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if fs := st[path]; fs != nil && fs.Worktree == git.Untracked {
			untracked++
			continue
		}
		from, err := gitBlobSide(base, path)
		if err != nil {
			return "", nil, err
		}
		to, dir := gitDiskSide(root, path)
		if dir || (from != nil && from.mode == filemode.Submodule) {
			notes = append(notes, "submodule "+path+" changed")
			continue
		}
		if from == nil && to == nil {
			continue // gone from both snapshots; nothing to show
		}
		writeGitFileDiff(&b, from, to)
	}
	if untracked > 0 {
		notes = append(notes, plural(untracked, "untracked file")+" not shown")
	}
	return strings.TrimRight(b.String(), "\n"), notes, nil
}

// changePath names a change's destination path, falling back to its source.
func changePath(ch *object.Change) string {
	if p := ch.To.Name; p != "" {
		return p
	}
	return ch.From.Name
}

// fileSide converts an object.File change side onto a gitSide, nil for absent.
func fileSide(f *object.File) (*gitSide, error) {
	if f == nil {
		return nil, nil
	}
	data, err := f.Contents()
	if err != nil {
		return nil, err
	}
	return &gitSide{path: f.Name, hash: f.Hash, mode: f.Mode, data: data, bin: binary([]byte(data))}, nil
}

// writeGitFileDiff appends one changed path as a git-shaped block: the
// diff --git header, then either the binary notice or hunks from the shared
// unified renderer.
func writeGitFileDiff(b *strings.Builder, from, to *gitSide) {
	if from == nil && to == nil {
		return
	}
	var head []string
	switch {
	case from == nil: // creation
		head = append(head, "diff --git a/"+to.path+" b/"+to.path,
			"new file mode "+gitMode(to.mode))
	case to == nil: // deletion
		head = append(head, "diff --git a/"+from.path+" b/"+from.path,
			"deleted file mode "+gitMode(from.mode))
	case from.path != to.path: // rename
		head = append(head, "diff --git a/"+from.path+" b/"+to.path,
			"rename from "+from.path, "rename to "+to.path)
	case from.mode != to.mode: // mode change only
		head = append(head, "diff --git a/"+from.path+" b/"+to.path,
			"old mode "+gitMode(from.mode), "new mode "+gitMode(to.mode))
	default:
		head = append(head, "diff --git a/"+from.path+" b/"+to.path)
	}
	if idx := gitIndexLine(from, to); idx != "" {
		head = append(head, idx)
	}

	var body string
	switch {
	case (from != nil && from.bin) || (to != nil && to.bin):
		body = "Binary files " + sideLabel("a/", "/dev/null", from) +
			" and " + sideLabel("b/", "/dev/null", to) + " differ"
	default:
		fromData, toData := "", ""
		if from != nil {
			fromData = from.data
		}
		if to != nil {
			toData = to.data
		}
		body = unifiedDiff(sideLabel("a/", "/dev/null", from), sideLabel("b/", "/dev/null", to), fromData, toData)
	}
	if body == "" && len(head) == 1 {
		return // no content or metadata change worth a block
	}
	b.WriteString(strings.Join(head, "\n") + "\n")
	if body != "" {
		b.WriteString(body + "\n")
	}
}

// sideLabel prefixes a side's path, or the /dev/null fallback when absent.
func sideLabel(prefix, none string, s *gitSide) string {
	if s == nil {
		return none
	}
	return prefix + s.path
}

// gitIndexLine renders git's index header, only where both hashes are
// repo-canonical, so a work-tree diff never prints one.
func gitIndexLine(from, to *gitSide) string {
	switch {
	case from == nil && to == nil:
		return ""
	case from == nil:
		if to.hash.IsZero() {
			return ""
		}
		return "index " + shortHash(plumbing.ZeroHash) + ".." + shortHash(to.hash)
	case to == nil:
		if from.hash.IsZero() {
			return ""
		}
		return "index " + shortHash(from.hash) + ".." + shortHash(plumbing.ZeroHash)
	case from.hash.IsZero() || to.hash.IsZero() || from.hash == to.hash:
		return ""
	default:
		line := "index " + shortHash(from.hash) + ".." + shortHash(to.hash)
		if from.mode == to.mode {
			line += " " + gitMode(from.mode)
		}
		return line
	}
}

// gitMode renders a mode the way git's diff headers do, without zero padding.
func gitMode(m filemode.FileMode) string {
	return fmt.Sprintf("%o", uint32(m))
}

// gitCommitDiffText renders the patch c introduces: first parent's tree to its
// own, or the empty tree for a root commit, limited to file's scope. A merge
// notes the first-parent basis, so the partial view is never mistaken for the
// whole change.
func gitCommitDiffText(ctx context.Context, c *object.Commit, file string) (string, error) {
	var parent *object.Commit
	if c.NumParents() > 0 {
		var err error
		if parent, err = c.Parent(0); err != nil {
			return "", err
		}
	}
	fromTree, err := gitTree(parent)
	if err != nil {
		return "", err
	}
	toTree, err := gitTree(c)
	if err != nil {
		return "", err
	}
	body, notes, err := gitDiffTreeText(ctx, fromTree, toTree, file)
	if err != nil {
		return "", err
	}
	if c.NumParents() > 1 {
		notes = append(notes, "merge commit; showing the diff against first parent "+shortHash(parent.ID()))
	}
	return finishDiff(body, notes), nil
}

// finishDiff joins a patch body with its notes, standing in "(no changes)" for
// an empty diff so a blank result reads as a real answer.
func finishDiff(body string, notes []string) string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		body = "(no changes)"
	}
	if len(notes) == 0 {
		return body
	}
	return body + "\n" + strings.Join(notes, "\n")
}

// gitHeadLine names the checked-out state: branch, detached HEAD or unborn.
func gitHeadLine(r *git.Repository) string {
	ref, err := r.Head()
	if err != nil {
		return "(no commits yet)"
	}
	if ref.Name() == plumbing.HEAD {
		return "HEAD detached at " + shortHash(ref.Hash())
	}
	return "on branch " + ref.Name().Short()
}

// shortHash abbreviates a hash the way git's default does.
func shortHash(h plumbing.Hash) string {
	s := h.String()
	return s[:min(7, len(s))]
}

// gitStatusLines renders st as git short-format lines sorted by path, plus a
// counts summary. Both-sides-unmodified entries (Preload artifacts) are
// skipped; a directory whose contents are all untracked collapses to one
// `?? dir/` line, and the untracked count follows the lines shown. tracked
// names HEAD's files: status omits clean entries, so they must come separately.
func gitStatusLines(st git.Status, tracked []string) (lines []string, staged, unstaged, untracked int) {
	paths := slices.Sorted(maps.Keys(st))
	type entry struct{ key, line string }
	var entries []entry
	var untrackedPaths []string
	blocked := bulk.SliceToSet(tracked) // anything tracked stops a collapse
	for _, path := range paths {
		fs := st[path]
		if fs.Staging == git.Unmodified && fs.Worktree == git.Unmodified {
			continue
		}
		if fs.Worktree == git.Untracked {
			untrackedPaths = append(untrackedPaths, path)
			continue
		}
		blocked[path] = struct{}{}
		name := path
		if fs.Extra != "" { // renames carry the previous name
			name = fs.Extra + " -> " + path
		}
		entries = append(entries, entry{path, fmt.Sprintf("%c%c %s", fs.Staging, fs.Worktree, name)})
		if fs.Staging != git.Unmodified {
			staged++
		}
		if fs.Worktree != git.Unmodified {
			unstaged++
		}
	}
	for _, dir := range collapseUntracked(blocked, untrackedPaths) {
		entries = append(entries, entry{dir, "?? " + dir})
		untracked++
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	for _, e := range entries {
		lines = append(lines, e.line)
	}
	return lines, staged, unstaged, untracked
}

// collapseUntracked maps each untracked path to its outermost directory with
// nothing blocked beneath it, or leaves the path alone when none exists, so a
// fresh directory reads as one `?? dir/` line like git status.
func collapseUntracked(blocked map[string]struct{}, untracked []string) []string {
	clean := func(dir string) bool { // nothing blocked under dir
		prefix := dir + "/"
		for p := range blocked {
			if strings.HasPrefix(p, prefix) {
				return false
			}
		}
		return true
	}
	var out []string
	seen := make(map[string]struct{}, len(untracked))
	for _, path := range untracked {
		chosen := path
		for dir := filepath.Dir(path); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			if !clean(dir) {
				break // ancestors contain the same tracked file: stop climbing
			}
			chosen = dir + "/"
		}
		if _, ok := seen[chosen]; !ok {
			seen[chosen] = struct{}{}
			out = append(out, chosen)
		}
	}
	slices.Sort(out)
	return out
}

// headTreePaths lists every path in HEAD's tree; empty before any commit.
func headTreePaths(r *git.Repository) ([]string, error) {
	c, err := gitCommit(r, "HEAD")
	if err != nil {
		if errors.Is(err, errNoCommits) {
			return nil, nil
		}
		return nil, err
	}
	t, err := c.Tree()
	if err != nil {
		return nil, err
	}
	var names []string
	_ = t.Files().ForEach(func(f *object.File) error {
		names = append(names, f.Name)
		return nil
	})
	return names, nil
}

// gitStatusText renders the full git_status output for r.
func gitStatusText(r *git.Repository) (string, error) {
	w, err := gitWorktree(r)
	if err != nil {
		return "", err
	}
	st, err := w.StatusWithOptions(git.StatusOptions{Strategy: git.Preload})
	if err != nil {
		return "", err
	}
	tracked, err := headTreePaths(r)
	if err != nil {
		return "", err
	}
	lines, staged, unstaged, untracked := gitStatusLines(st, tracked)

	var b strings.Builder
	b.WriteString(gitHeadLine(r) + "\n")
	var counts []string
	for _, c := range []struct {
		n int
		s string
	}{
		{staged, "staged"},
		{unstaged, "unstaged"},
		{untracked, "untracked"},
	} {
		if c.n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", c.n, c.s))
		}
	}
	if len(counts) == 0 {
		b.WriteString("(clean)\n")
	} else {
		b.WriteString(strings.Join(counts, ", ") + "\n")
	}
	for _, ln := range lines {
		b.WriteString(ln + "\n")
	}
	return b.String(), nil
}
