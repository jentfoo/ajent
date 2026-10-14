package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// errNotGit reports that path is not inside a git work tree.
var errNotGit = errors.New("not a git repository")

// errNoCommits reports an unborn HEAD: the repository exists but is empty.
var errNoCommits = errors.New("repository has no commits yet")

// git_* engine: go-git, in process only. No subprocess, never exec of the git
// binary, so filters, hooks and pagers in a hostile work tree cannot run.

// sharedGitCache bounds object reads across every repository a tool opens.
// Object hashes are content-addressed, so one process-wide LRU is safe to share
// between repos and calls: the same hash always names identical bytes. On slow
// filesystems this turns repeated log/show/diff walks into in-memory hits.
var sharedGitCache = cache.NewObjectLRU(128 * cache.MiByte)

// openGitRepo opens the repository containing path with the process-shared
// object cache, so a turn's several git_* calls on one repo read each object
// from disk once instead of per call.
func openGitRepo(path string) (*git.Repository, error) {
	dotRoot, wtRoot := findDotGit(path)
	if dotRoot == "" { // not under any .git: treat path itself as a bare repository
		dotRoot = path
	}
	s := filesystem.NewStorage(repoFilesystem(dotRoot), sharedGitCache)
	var (
		r   *git.Repository
		err error
	)
	if wtRoot != "" {
		r, err = git.Open(s, osfs.New(wtRoot))
	} else { // bare: no work tree
		r, err = git.Open(s, nil)
	}
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, errNotGit
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// findDotGit walks up from path looking for a .git entry (directory or a
// "gitdir:" pointer file), returning that entry's directory and the work-tree
// root. For a linked worktree the directory is the per-worktree metadata dir,
// not the shared object store; openGitRepo resolves those via commondir.
// An empty dotRoot means nothing was found.
func findDotGit(path string) (dotRoot, wtRoot string) {
	p := path
	for {
		gp := filepath.Join(p, ".git")
		fi, err := os.Stat(gp)
		switch {
		case err == nil && fi.IsDir():
			return gp, p
		case err == nil: // linked worktree or submodule pointer file
			b, rerr := os.ReadFile(gp)
			if rerr != nil {
				break
			}
			line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
			target := strings.TrimPrefix(line, "gitdir: ")
			if target == line { // not a gitdir pointer; treat as not found
				break
			}
			if filepath.IsAbs(target) {
				return target, p
			}
			return filepath.Join(p, target), p
		}
		next := filepath.Dir(p)
		if next == p { // reached filesystem root without finding .git
			break
		}
		p = next
	}
	return "", ""
}

// repoFilesystem returns the storage filesystem to read dotRoot through. A
// linked-worktree metadata dir is merged with its commondir: objects and
// shared refs resolve there, while HEAD, index and per-worktree refs stay in
// the metadata directory.
func repoFilesystem(dotRoot string) billy.Filesystem {
	if common := worktreeCommonDir(dotRoot); common != "" {
		return dotgit.NewRepositoryFilesystem(osfs.New(dotRoot), osfs.New(common))
	}
	return osfs.New(dotRoot)
}

// worktreeCommonDir returns the shared storage directory a linked-worktree
// metadata dir names in its commondir file, resolved against dotRoot when
// relative. Empty means dotRoot holds the object store itself.
func worktreeCommonDir(dotRoot string) string {
	b, err := os.ReadFile(filepath.Join(dotRoot, "commondir"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return ""
	}
	if filepath.IsAbs(line) {
		return filepath.Clean(line)
	}
	return filepath.Join(dotRoot, line)
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

// gitBlobCeiling caps how much of one side's content a diff materializes.
// Text past it is withheld with an explicit note instead of hunks over a
// truncated view, so one huge blob cannot dominate memory or CPU. Binary
// detection only needs the prefix either way.
const gitBlobCeiling = 512 << 10

// gitSide is one snapshot of a path: blob content plus repo metadata. nil is
// the /dev/null side of a creation or deletion.
type gitSide struct {
	path string
	hash plumbing.Hash // zero for the work-tree side, where the hash is not repo-canonical
	mode filemode.FileMode
	size int64  // full content size, past len(data) when cut
	data string // up to gitBlobCeiling bytes; the whole blob when !cut
	cut  bool   // data stopped at the ceiling: hunks withheld, never partial
	bin  bool
}

// readGitSide reads r up to gitBlobCeiling bytes, reporting whether content
// remains past what it returned.
func readGitSide(r io.Reader) (data string, cut bool, err error) {
	raw, err := io.ReadAll(io.LimitReader(r, gitBlobCeiling+1))
	if err != nil {
		return "", false, err
	}
	if cut = len(raw) > gitBlobCeiling; cut {
		raw = raw[:gitBlobCeiling]
	}
	return string(raw), cut, nil
}

// gitBlobSide reads path out of a tree. A missing entry or submodule yields
// nil with no error, while a read failure returns the error so a broken object is
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
	return fileSide(f)
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
	var cut bool
	if fi.Mode()&os.ModeSymlink != 0 { // symlink blobs hold the target path
		data, err = os.Readlink(full)
	} else {
		f, openErr := os.Open(full)
		if openErr != nil {
			return nil, false
		}
		data, cut, err = readGitSide(f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return nil, false
	}
	mode, err := filemode.NewFromOSFileMode(fi.Mode())
	if err != nil {
		return nil, false
	}
	return &gitSide{path: path, mode: mode, size: fi.Size(), data: data, cut: cut, bin: binary([]byte(data))}, false
}

// normalizeGitPathspec cleans a model-supplied repo-relative pathspec once at
// parameter parse, so equivalent spellings share one scope ("dir/", "./dir"
// and "dir/." all select dir) and can never filter everything out. An empty
// result matches everything; specs escaping the repo ("..") stay unmatched.
func normalizeGitPathspec(file string) string {
	p := strings.TrimSpace(filepath.ToSlash(file))
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	p = strings.TrimLeft(p, "/") // an absolute spelling still means repo-relative
	if p == "." || p == "" {
		return ""
	}
	return p
}

// gitPathScope matches file exactly or anything beneath it as a directory.
// An empty scope matches everything. file must come from normalizeGitPathspec,
// so it carries no trailing separator or dot segments.
func gitPathScope(file string) func(string) bool {
	if file == "" {
		return func(string) bool { return true }
	}
	return func(path string) bool {
		return path == file || strings.HasPrefix(path, file+"/")
	}
}

// gitDiffOut renders one diff under GitResultLimit: per-file blocks stream
// into a kept head and everything past the bound flows straight to a spill
// file, so peak memory is one block rather than the whole patch. Once full,
// callers stop expensive content reads for paths that cannot appear anymore.
type gitDiffOut struct {
	head    bytes.Buffer // kept prefix within the limit
	spill   *spiller     // lazily created file holding the complete stream
	w       *boundedWriter
	notes   []string // deferred footer notes, order preserved
	shown   int      // blocks emitted
	skipped int      // in-scope paths dropped once full
}

// newGitDiffOut names its spill file after prefix.
func newGitDiffOut(sessionID, prefix string) *gitDiffOut {
	d := &gitDiffOut{spill: newSpiller(sessionID, prefix)}
	d.w = Writer(&d.head, GitResultLimit(), d.spill).(*boundedWriter)
	return d
}

// Write appends raw text such as git_show's metadata header.
func (d *gitDiffOut) Write(p []byte) (int, error) { return d.w.Write(p) }

// Note defers one footer line.
func (d *gitDiffOut) Note(s string) { d.notes = append(d.notes, s) }

// Full reports whether the result bound was reached: stop reading content for
// paths that can no longer appear in the output.
func (d *gitDiffOut) Full() bool { return d.w.Truncated() }

// Block appends one changed path as a git-shaped block. Callers check Full
// before materializing sides; skipped counting stays with them.
func (d *gitDiffOut) Block(from, to *gitSide) {
	var b strings.Builder
	writeGitFileDiff(&b, from, to)
	if b.Len() == 0 {
		return // no content or metadata change worth a block
	}
	_, _ = d.w.Write([]byte(b.String()))
	d.shown++
}

// Text flushes the stream and returns the model text: kept blocks or the
// "(no changes)" stand-in, deferred notes, then a truncation footer describing
// what was cut.
func (d *gitDiffOut) Text(paging string) string {
	d.w.Flush()
	body := strings.TrimRight(d.head.String(), "\n")
	if d.head.Len() == 0 && !d.Full() { // show's metadata header must survive
		body = "(no changes)"
	}
	if d.skipped > 0 {
		d.Note(plural(d.skipped, "more changed path") + " not shown")
	}
	if len(d.notes) > 0 {
		footer := strings.Join(d.notes, "\n")
		if body == "" {
			body = footer
		} else {
			body += "\n" + footer
		}
	}
	truncated := d.Full() || overlongLine(body)
	if !truncated {
		return body
	}
	capped := capText(body)
	if !d.Full() { // only an overlong line triggered this: no file yet,
		// write the complete stream so read can recover the uncapped tail
		_, _ = d.spill.Write(d.w.replay.Bytes())
	}
	lines, bytes := d.w.Total()
	note := truncationNote(Bounded{Shown: countLines(capped), Lines: lines, Bytes: bytes}, d.spill.claim(), paging)
	if capped == "" {
		return note // nothing fit at all, the footer is the whole answer
	}
	return capped + "\n" + note
}

// close flushes and closes the spill file when one was created.
func (d *gitDiffOut) close() error { return d.spill.close() }

// gitDiffTreeText renders the rename-aware unified patch between two trees as
// git-shaped per-file blocks into d, limited to file's path scope when
// non-empty. Rename detection runs on the in-scope changes only, so a scoped
// call never pays similarity reads for paths it will not render.
// Notes name omitted content: submodules.
func gitDiffTreeText(ctx context.Context, from, to *object.Tree, file string, d *gitDiffOut) error {
	changes, err := object.DiffTreeWithOptions(ctx, from, to, &object.DiffTreeOptions{})
	if err != nil {
		return err
	}
	if file != "" { // raw adds/deletes/modifies carry in-scope names only,
		// so detection afterward can never pair a change across the scope
		keep := gitPathScope(file)
		changes = bulk.SliceFilter(func(ch *object.Change) bool {
			return keep(ch.From.Name) || keep(ch.To.Name)
		}, changes)
	}
	if changes, err = object.DetectRenames(changes, object.DefaultDiffTreeOptions); err != nil {
		return err
	}
	for _, ch := range changes { // every surviving side is in scope
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Full() {
			d.skipped++
			continue
		}
		fromFile, toFile, err := ch.Files()
		if err != nil {
			return err
		}
		if (fromFile != nil && fromFile.Mode == filemode.Submodule) ||
			(toFile != nil && toFile.Mode == filemode.Submodule) {
			d.Note("submodule " + changePath(ch) + " changed")
			continue
		}
		from, err := fileSide(fromFile)
		if err != nil {
			return err
		}
		to, err := fileSide(toFile)
		if err != nil {
			return err
		}
		d.Block(from, to)
	}
	return nil
}

// gitDiffWorktreeText renders committed state at base against the files on
// disk into d, like `git diff <ref>`: every difference since base, committed or
// not, limited to file's path scope when non-empty. Notes name untracked files,
// which git does not diff either.
func gitDiffWorktreeText(ctx context.Context, r *git.Repository, base *object.Tree, file string, d *gitDiffOut) error {
	w, err := gitWorktree(r)
	if err != nil {
		return err
	}
	st, err := w.StatusWithOptions(git.StatusOptions{Strategy: git.Preload})
	if err != nil {
		return err
	}
	root := w.Filesystem.Root()

	keep := gitPathScope(file) // matches everything when file is empty
	paths := make(map[string]struct{}, len(st))
	// seedPaths adds a tree's in-scope file names; walk errors propagate so a
	// broken tree never reads as absent files
	seedPaths := func(t *object.Tree) error {
		return t.Files().ForEach(func(f *object.File) error {
			if keep(f.Name) {
				paths[f.Name] = struct{}{}
			}
			return nil
		})
	}
	if err := seedPaths(base); err != nil {
		return err
	}
	// status entries are HEAD-relative: files committed since base but clean
	// against HEAD appear in none, so seed HEAD's tree too
	head, err := gitCommit(r, "HEAD")
	if err != nil && !errors.Is(err, errNoCommits) {
		return err
	}
	if head != nil && head.TreeHash != base.Hash { // identical trees add nothing
		headTree, err := gitTree(head)
		if err != nil {
			return err
		}
		if err := seedPaths(headTree); err != nil {
			return err
		}
	}
	for path := range st {
		if keep(path) {
			paths[path] = struct{}{}
		}
	}

	var untracked int
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// counted before the full check: status knows it cheaply, and the
		// footer names untracked files even when rendering stopped early
		if fs := st[path]; fs != nil && fs.Worktree == git.Untracked {
			untracked++
			continue
		}
		if d.Full() {
			d.skipped++
			continue
		}
		from, err := gitBlobSide(base, path)
		if err != nil {
			return err
		}
		to, dir := gitDiskSide(root, path)
		if dir || (from != nil && from.mode == filemode.Submodule) {
			d.Note("submodule " + path + " changed")
			continue
		}
		if from == nil && to == nil {
			continue // gone from both snapshots, nothing to show
		}
		d.Block(from, to)
	}
	if untracked > 0 {
		d.Note(plural(untracked, "untracked file") + " not shown")
	}
	return nil
}

// changePath names a change's destination path, falling back to its source.
func changePath(ch *object.Change) string {
	if p := ch.To.Name; p != "" {
		return p
	}
	return ch.From.Name
}

// fileSide converts an object.File change side onto a gitSide, nil for absent.
// Content past gitBlobCeiling is not read; the cut side withholds hunks later.
func fileSide(f *object.File) (*gitSide, error) {
	if f == nil {
		return nil, nil
	}
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	data, cut, rerr := readGitSide(r)
	if cerr := r.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return nil, rerr
	}
	return &gitSide{path: f.Name, hash: f.Hash, mode: f.Mode, size: f.Size, data: data, cut: cut, bin: binary([]byte(data))}, nil
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
	case (from != nil && from.cut) || (to != nil && to.cut):
		// hunks over truncated content would read as the whole change
		n := int64(0)
		if from != nil {
			n = max(n, from.size)
		}
		if to != nil {
			n = max(n, to.size)
		}
		body = "Contents too large to show (" + strutil.HumanSize(n) + ")"
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

// gitCommitDiffText renders the patch c introduces into d: first parent's tree
// to its own, or the empty tree for a root commit, limited to file's scope. A
// merge notes the first-parent basis, so the partial view is never mistaken for
// the whole change.
func gitCommitDiffText(ctx context.Context, c *object.Commit, file string, d *gitDiffOut) error {
	var parent *object.Commit
	if c.NumParents() > 0 {
		var err error
		if parent, err = c.Parent(0); err != nil {
			return err
		}
	}
	fromTree, err := gitTree(parent)
	if err != nil {
		return err
	}
	toTree, err := gitTree(c)
	if err != nil {
		return err
	}
	if err := gitDiffTreeText(ctx, fromTree, toTree, file, d); err != nil {
		return err
	}
	if c.NumParents() > 1 {
		d.Note("merge commit; showing the diff against first parent " + shortHash(parent.ID()))
	}
	return nil
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
// skipped. A directory whose contents are all untracked collapses to one
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
	// ancestor directories of every blocked path, for O(1) prefix lookups
	blockedDirs := make(map[string]struct{}, len(blocked))
	for p := range blocked {
		for dir := filepath.Dir(p); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			blockedDirs[dir] = struct{}{}
		}
	}
	clean := func(dir string) bool { // nothing blocked under dir
		_, ok := blockedDirs[dir]
		return !ok
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

// headTreePaths lists every path in HEAD's tree, empty before any commit.
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
