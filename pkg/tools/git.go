package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
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
	// go-git reads HEAD, config, index, refs, objects and .gitmodules through
	// its own opens, so both filesystems pass the special-file guard
	s := filesystem.NewStorage(guardedFS{repoFilesystem(dotRoot)}, sharedGitCache)
	var (
		r   *git.Repository
		err error
	)
	if wtRoot != "" {
		r, err = git.Open(s, guardedFS{osfs.New(wtRoot)})
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
			if specialFile(fi.Mode()) {
				break
			}
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

// guardedFS refuses to open a pipe, device or socket before delegating, so a
// hostile repository cannot hang a git_* tool inside go-git's own reads
type guardedFS struct{ billy.Filesystem }

// Open routes through OpenFile so one guard covers both entry points
func (f guardedFS) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile reports the shared special-file refusal. Stat runs through the
// wrapped fs so chroot and commondir path mapping still apply, and follows
// symlinks: a link to a regular file reads, a link to a fifo refuses
func (f guardedFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if fi, err := f.Stat(name); err == nil && specialFile(fi.Mode()) {
		return nil, &fs.PathError{Op: "open", Path: f.Join(f.Root(), name), Err: errNotRegular}
	}
	return f.Filesystem.OpenFile(name, flag, perm)
}

// worktreeCommonDir returns the shared storage directory a linked-worktree
// metadata dir names in its commondir file, resolved against dotRoot when
// relative. Empty means dotRoot holds the object store itself.
func worktreeCommonDir(dotRoot string) string {
	p := filepath.Join(dotRoot, "commondir")
	if refuseSpecial(p) != nil {
		return ""
	}
	b, err := os.ReadFile(p)
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

// gitBlobSide reads path out of a tree: nil when absent, a hash-only side
// for a submodule gitlink (it references a commit, not a blob), content for
// files. A read failure returns the error so a broken object is never misread
// as a deletion.
func gitBlobSide(t *object.Tree, path string) (*gitSide, error) {
	if t == nil {
		return nil, nil
	}
	e, err := t.FindEntry(path)
	if err != nil {
		// absent leaf or absent parent directory both read as absent
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if e.Mode == filemode.Submodule {
		return &gitSide{path: path, hash: e.Hash, mode: e.Mode}, nil
	}
	f, err := t.TreeEntryFile(e)
	if err != nil {
		return nil, err
	}
	return fileSide(f)
}

// gitDiskSide reads one path off the work tree. dir reports a directory at
// path, most plausibly an unopened submodule checkout. A missing, unreadable
// or special (pipe, device, socket) file yields a nil side, the /dev/null side
// of a deletion.
func gitDiskSide(root, path string) (side *gitSide, dir bool) {
	full := filepath.Join(root, filepath.FromSlash(path))
	fi, err := os.Lstat(full)
	if err != nil {
		return nil, false
	}
	if fi.IsDir() {
		return nil, true
	}
	if specialFile(fi.Mode()) {
		return nil, false
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
	metas   []string // view notes joined past the bound, order preserved
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

// Meta defers one view note (merge basis, untracked or skipped counts) past
// the bound, like the truncation footer: the explanation survives even when
// the content it describes did not.
func (d *gitDiffOut) Meta(s string) { d.metas = append(d.metas, s) }

// Full reports whether the result bound was reached: stop reading content for
// paths that can no longer appear in the output.
func (d *gitDiffOut) Full() bool { return d.w.Truncated() }

// EmptyPatch reports a genuinely empty patch section: no block rendered and
// none dropped to the bound. A full writer with skipped paths means changes
// existed, only the budget did not.
func (d *gitDiffOut) EmptyPatch() bool {
	return d.shown == 0 && d.skipped == 0
}

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

// Text flushes the stream and returns the model text: kept blocks and notes
// or the "(no changes)" stand-in, view notes, then a truncation footer
// describing what was cut.
func (d *gitDiffOut) Text(paging string) string {
	d.w.Flush()
	body := strings.TrimRight(d.head.String(), "\n")
	if d.head.Len() == 0 && !d.Full() { // show's metadata header must survive
		body = "(no changes)"
	}
	if d.skipped > 0 {
		d.Meta(strutil.Plural(d.skipped, "more changed path") + " not shown")
	}
	if len(d.metas) > 0 {
		footer := strings.Join(d.metas, "\n")
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

// isBlobMode reports a plain file mode, not a symlink or gitlink.
func isBlobMode(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable
}

// detectGitRenames pairs renames the way git does, on content alone. It wraps
// go-git's detector, which rejects pairs whose file modes differ and would
// split a rename with a chmod into separate delete and add blocks. Returned
// sides keep their original modes.
func detectGitRenames(changes object.Changes) (object.Changes, error) {
	fromModes := make(map[string]filemode.FileMode, len(changes))
	toModes := make(map[string]filemode.FileMode, len(changes))
	normalized := make(object.Changes, len(changes))
	for i, ch := range changes {
		if ch.From.Name != "" {
			fromModes[ch.From.Name] = ch.From.TreeEntry.Mode
		}
		if ch.To.Name != "" {
			toModes[ch.To.Name] = ch.To.TreeEntry.Mode
		}
		c := *ch // copy, so the caller's entries are never rewritten
		if isBlobMode(c.From.TreeEntry.Mode) {
			c.From.TreeEntry.Mode = filemode.Regular
		}
		if isBlobMode(c.To.TreeEntry.Mode) {
			c.To.TreeEntry.Mode = filemode.Regular
		}
		normalized[i] = &c
	}
	paired, err := object.DetectRenames(normalized, object.DefaultDiffTreeOptions)
	if err != nil {
		return nil, err
	}
	for _, ch := range paired { // every non-empty side names an input path
		if ch.From.Name != "" {
			ch.From.TreeEntry.Mode = fromModes[ch.From.Name]
		}
		if ch.To.Name != "" {
			ch.To.TreeEntry.Mode = toModes[ch.To.Name]
		}
	}
	return paired, nil
}

// pairedTreeChanges diffs two trees and pairs renames on content alone.
func pairedTreeChanges(ctx context.Context, from, to *object.Tree) (object.Changes, error) {
	raw, err := object.DiffTreeWithOptions(ctx, from, to, &object.DiffTreeOptions{})
	if err != nil {
		return nil, err
	}
	return detectGitRenames(raw)
}

// gitDiffTreeText renders the rename-aware unified patch between two trees as
// git-shaped per-file blocks into d, limited to file's path scope when
// non-empty. Rename detection runs on the in-scope changes only, so a scoped
// call never pays similarity reads for paths it will not render.
// Gitlinks render per sub, git's --submodule forms.
func gitDiffTreeText(ctx context.Context, from, to *object.Tree, file string, sub submoduleRender, d *gitDiffOut) error {
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
	if changes, err = detectGitRenames(changes); err != nil {
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
		// Files() yields nils for gitlink sides, so modes come from the entries
		if ch.From.TreeEntry.Mode == filemode.Submodule || ch.To.TreeEntry.Mode == filemode.Submodule {
			if err := renderTreeGitlink(ctx, d, sub, ch); err != nil {
				return err
			}
			continue
		}
		fromFile, toFile, err := ch.Files()
		if err != nil {
			return err
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

// gitlinkRef carries one gitlink path's base-tree and index pointers, the
// two snapshots a work-tree diff compares.
type gitlinkRef struct {
	base, idx plumbing.Hash
}

// renderTreeGitlink emits one tree-diff gitlink change: a pure pointer change
// renders in the chosen style, while a file-to-gitlink mode flip keeps the
// short form, the blob side's real content beside the synthesized one.
func renderTreeGitlink(ctx context.Context, d *gitDiffOut, sub submoduleRender, ch *object.Change) error {
	from, to := ch.From.TreeEntry, ch.To.TreeEntry
	pure := (from.Mode == filemode.Submodule || from.Mode == filemode.Empty) &&
		(to.Mode == filemode.Submodule || to.Mode == filemode.Empty)
	if pure {
		return renderGitlink(ctx, d, sub, changePath(ch), from.Hash, to.Hash)
	}
	fromFile, toFile, err := ch.Files()
	if err != nil {
		return err
	}
	fs, err := fileSide(fromFile)
	if err != nil {
		return err
	}
	ts, err := fileSide(toFile)
	if err != nil {
		return err
	}
	if from.Mode == filemode.Submodule {
		fs = gitlinkSide(from.Name, from.Hash)
	}
	if to.Mode == filemode.Submodule {
		ts = gitlinkSide(to.Name, to.Hash)
	}
	d.Block(fs, ts)
	return nil
}

// gitDiffWorktreeText renders committed state at base against the files on
// disk into d, like `git diff <ref>`: every difference since base, committed or
// not, limited to file's path scope when non-empty. Notes name untracked files,
// which git does not diff either; gitlinks render per sub.
func gitDiffWorktreeText(ctx context.Context, r *git.Repository, base *object.Tree, file string, sub submoduleRender, d *gitDiffOut) error {
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
	baseLinks := make(map[string]plumbing.Hash)
	// seedPaths adds a tree's in-scope paths, erroring on a broken tree so it
	// never reads as absent files. Gitlinks reference commits, not blobs, so
	// their pointers are recorded when links is non-nil.
	seedPaths := func(t *object.Tree, links map[string]plumbing.Hash) error {
		return walkTreePaths(r.Storer, t, func(path string, e object.TreeEntry) error {
			if !keep(path) {
				return nil
			}
			paths[path] = struct{}{}
			if e.Mode == filemode.Submodule && links != nil {
				links[path] = e.Hash
			}
			return nil
		})
	}
	if err := seedPaths(base, baseLinks); err != nil {
		return err
	}
	// status entries are HEAD-relative: files committed since base but clean
	// against HEAD appear in none, so seed HEAD's tree too. Identical trees
	// share every entry; gitlinks there only widen path discovery.
	head, err := gitCommit(r, "HEAD")
	if err != nil && !errors.Is(err, errNoCommits) {
		return err
	}
	if head != nil && head.TreeHash != base.Hash {
		headTree, err := gitTree(head)
		if err != nil {
			return err
		}
		if err := seedPaths(headTree, nil); err != nil {
			return err
		}
	}
	for path := range st {
		if keep(path) {
			paths[path] = struct{}{}
		}
	}
	idxLinks, err := indexGitlinks(r)
	if err != nil {
		return err
	}
	links := make(map[string]gitlinkRef, len(baseLinks)+len(idxLinks))
	for p, h := range baseLinks {
		links[p] = gitlinkRef{base: h}
	}
	for p, h := range idxLinks {
		if !keep(p) {
			continue
		}
		ref := links[p] // zero base when base lacks the link
		ref.idx = h
		links[p] = ref
		paths[p] = struct{}{}
	}
	// checkouts hold the commit each index-tracked gitlink is checked out at,
	// resolved through the nested repository so declared and undeclared
	// submodules report alike: go-git's status only takes its submodule-aware
	// path for ones declared in .gitmodules. Links gone from the index skip
	// resolution: their checkouts are leftovers of a staged deletion.
	checkouts := make(map[string]plumbing.Hash, len(links))
	for p, ref := range links {
		if ref.idx.IsZero() {
			continue
		}
		if h := gitCheckoutHash(root, p); !h.IsZero() {
			checkouts[p] = h
		}
	}

	var untracked int
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// a gitlink path is never genuinely untracked, even when the index
		// dropped it and status keys the leftover directory as one
		if ref, ok := links[path]; ok {
			if d.Full() {
				d.skipped++
				continue
			}
			// pointer moves render, never diff file content. The staged index
			// pointer is the work-tree side, and a checkout's commit wins while
			// the entry is still staged.
			to := ref.idx
			if h, checkedOut := checkouts[path]; checkedOut {
				to = h
			}
			if ref.base != to {
				if err := renderGitlink(ctx, d, sub, path, ref.base, to); err != nil {
					return err
				}
			}
			continue
		}
		fs := st[path]
		// counted before the full check: status knows it cheaply, and the
		// footer names untracked files even when rendering stopped early
		if fs != nil && fs.Worktree == git.Untracked {
			if beneathGitlink(checkouts, path) {
				continue // contents of a checkout belong to its repository
			}
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
		if dir { // a directory over a tracked file reads as its deletion
			d.Block(from, nil)
			continue
		}
		if from == nil && to == nil {
			continue // gone from both snapshots, nothing to show
		}
		d.Block(from, to)
	}
	if untracked > 0 {
		d.Meta(strutil.Plural(untracked, "untracked file") + " not shown")
	}
	return nil
}

// indexGitlinks returns the index's gitlink entries as path to commit.
func indexGitlinks(r *git.Repository) (map[string]plumbing.Hash, error) {
	idx, err := r.Storer.Index()
	if err != nil {
		return nil, err
	}
	var links map[string]plumbing.Hash
	for _, e := range idx.Entries {
		if e.Mode != filemode.Submodule {
			continue
		}
		if links == nil {
			links = make(map[string]plumbing.Hash, 4)
		}
		links[e.Name] = e.Hash
	}
	return links, nil
}

// gitCheckoutRepo opens the repository checked out at a gitlink path in root's
// work tree, when one is. Reads stay behind the same guard chain as the
// parent's; missing, unreadable and nested-less directories all read as absent.
func gitCheckoutRepo(root, path string) (*git.Repository, bool) {
	if root == "" {
		return nil, false
	}
	sub := filepath.Join(root, filepath.FromSlash(path))
	if _, wt := findDotGit(sub); wt != sub { // the .git found must be path's own
		return nil, false
	}
	r, err := openGitRepo(sub)
	if err != nil {
		return nil, false
	}
	return r, true
}

// gitCheckoutHash resolves the commit a gitlink at path is checked out at in
// root's work tree, zero when no repository is checked out there.
func gitCheckoutHash(root, path string) plumbing.Hash {
	r, ok := gitCheckoutRepo(root, path)
	if !ok {
		return plumbing.ZeroHash
	}
	ref, err := r.Head()
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// renderChildDiff streams the child's own patch between two pointers, paths
// prefixed by the submodule's, as git's --submodule=diff does. Nested gitlinks
// render in the short form: git does not recurse.
func renderChildDiff(ctx context.Context, d *gitDiffOut, r *git.Repository, path string, from, to plumbing.Hash) error {
	fromTree := &object.Tree{}
	if !from.IsZero() {
		fc, err := r.CommitObject(from)
		if err != nil {
			return err
		}
		if fromTree, err = fc.Tree(); err != nil {
			return err
		}
	}
	toTree := &object.Tree{}
	if !to.IsZero() {
		tc, err := r.CommitObject(to)
		if err != nil {
			return err
		}
		if toTree, err = tc.Tree(); err != nil {
			return err
		}
	}
	changes, err := pairedTreeChanges(ctx, fromTree, toTree)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if d.Full() {
			d.skipped++
			continue
		}
		fromFile, toFile, err := ch.Files()
		if err != nil {
			return err
		}
		from, err := fileSide(fromFile)
		if err != nil {
			return err
		}
		to, err := fileSide(toFile)
		if err != nil {
			return err
		}
		// Files() yields nils for gitlink sides, so those synthesize
		if ch.From.TreeEntry.Mode == filemode.Submodule {
			from = gitlinkSide(path+"/"+ch.From.Name, ch.From.TreeEntry.Hash)
		} else if from != nil {
			from.path = path + "/" + from.path
		}
		if ch.To.TreeEntry.Mode == filemode.Submodule {
			to = gitlinkSide(path+"/"+ch.To.Name, ch.To.TreeEntry.Hash)
		} else if to != nil {
			to.path = path + "/" + to.path
		}
		d.Block(from, to)
	}
	return nil
}

// beneathGitlink reports whether path sits inside one of the checked-out
// gitlinks: such contents belong to the nested repository, never this work
// tree's untracked set.
func beneathGitlink(checkouts map[string]plumbing.Hash, path string) bool {
	for p := range checkouts {
		if strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// submoduleStyle selects how git_diff and git_show render gitlink changes,
// mirroring git's --submodule option.
type submoduleStyle uint8

const (
	submoduleShort submoduleStyle = iota // Subproject commit hunks, git's default
	submoduleLog                         // child commit subjects between the pointers
	submoduleDiff                        // the child's own patch between the pointers
)

// parseSubmoduleStyle maps the model-facing submodule argument onto its style,
// "" meaning git's default.
func parseSubmoduleStyle(s string) (submoduleStyle, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "short":
		return submoduleShort, nil
	case "log":
		return submoduleLog, nil
	case "diff":
		return submoduleDiff, nil
	default:
		return 0, fmt.Errorf("submodule must be short, log or diff, got %q", s)
	}
}

// submoduleRender carries one call's gitlink rendering style plus the work-tree
// root the log and diff forms resolve nested checkouts through; an empty root
// leaves them git's uninitialized fallback.
type submoduleRender struct {
	style submoduleStyle
	root  string
}

// newSubmoduleRender builds the renderer for r, with no work tree to resolve
// nested checkouts through when r is bare.
func newSubmoduleRender(r *git.Repository, style submoduleStyle) submoduleRender {
	sub := submoduleRender{style: style}
	if w, err := r.Worktree(); err == nil {
		sub.root = w.Filesystem.Root()
	}
	return sub
}

// gitlinkSide synthesizes one gitlink change side: git renders a pointer as a
// one-line "Subproject commit" pseudo-blob, nil for an absent side.
func gitlinkSide(path string, hash plumbing.Hash) *gitSide {
	if hash.IsZero() {
		return nil
	}
	data := "Subproject commit " + hash.String() + "\n"
	return &gitSide{path: path, hash: hash, mode: filemode.Submodule, size: int64(len(data)), data: data}
}

// renderGitlink emits one pointer change in the chosen style, byte-faithful to
// git's --submodule forms. A zero side is an add or a deletion; log and diff
// fall back to git's uninitialized wording when no nested checkout resolves
// the child commits.
func renderGitlink(ctx context.Context, d *gitDiffOut, sub submoduleRender, path string, from, to plumbing.Hash) error {
	if sub.style == submoduleShort {
		d.Block(gitlinkSide(path, from), gitlinkSide(path, to))
		return nil
	}
	d.shown++ // header and subjects render in place of a block
	child, hasRepo := gitCheckoutRepo(sub.root, path)
	var delta submoduleDelta
	var capped, resolved bool
	switch {
	case !hasRepo:
		// no checkout holds the child: git's uninitialized wording below
	case from.IsZero() || to.IsZero():
		resolved = true // an add or deletion holds no subjects to walk
	default:
		if wd, capHit, ok := walkSubmoduleDelta(child, from, to); ok {
			delta, capped, resolved = wd, capHit, true
		}
	}
	var b strings.Builder
	switch {
	case from.IsZero():
		fmt.Fprintf(&b, "Submodule %s 0000000...%s (new submodule)\n", path, shortHash(to))
	case to.IsZero():
		fmt.Fprintf(&b, "Submodule %s %s...0000000 (submodule deleted)\n", path, shortHash(from))
	case !resolved:
		fmt.Fprintf(&b, "Submodule %s %s...%s (commits not present)\n", path, shortHash(from), shortHash(to))
	case capped: // relation unknown: git's far-apart shape claims neither
		fmt.Fprintf(&b, "Submodule %s %s...%s:\n", path, shortHash(from), shortHash(to))
	case delta.rewind: // connected, moving backwards
		fmt.Fprintf(&b, "Submodule %s %s..%s (rewind):\n", path, shortHash(from), shortHash(to))
	case delta.fastForward: // connected, moving forwards: git still prints two dots
		fmt.Fprintf(&b, "Submodule %s %s..%s:\n", path, shortHash(from), shortHash(to))
	default: // diverged tips
		fmt.Fprintf(&b, "Submodule %s %s...%s:\n", path, shortHash(from), shortHash(to))
	}
	if sub.style == submoduleLog {
		// git's diff form prints the header alone
		for _, c := range delta.newOnly {
			b.WriteString("  > " + commitSubject(c) + "\n")
		}
		for _, c := range delta.oldOnly {
			b.WriteString("  < " + commitSubject(c) + "\n")
		}
		if capped {
			b.WriteString("  ... more commits not walked\n")
		}
	}
	_, _ = d.w.Write([]byte(b.String()))
	if sub.style == submoduleDiff && resolved {
		return renderChildDiff(ctx, d, child, path, from, to)
	}
	return nil
}

// submoduleDelta holds the commits each side of a child pointer pair alone
// holds, newest first, and how the pointers relate when the walk finished.
type submoduleDelta struct {
	newOnly, oldOnly []*object.Commit
	fastForward      bool // the new pointer descends from the old one
	rewind           bool // the old pointer descends from the new one
}

// gitSubmoduleWalkMax bounds the commit reads one pointer walk pays, so a
// huge or hostile child history cannot turn a log-form header into a
// full-history read on a slow filesystem.
var gitSubmoduleWalkMax = 512

// walkSubmoduleDelta walks both pointer histories in lockstep until the waves
// meet at a common ancestor, collecting the commits each side alone holds and
// how the pointers relate. Cost tracks the distance between the pointers, not
// the child's history depth. capped reports the visit bound cutting the walk
// short, which leaves the relation unknown and the listing without its tail.
// A commit that fails to load reads as unresolved, never as absent history.
func walkSubmoduleDelta(r *git.Repository, from, to plumbing.Hash) (d submoduleDelta, capped, ok bool) {
	const (
		sideFrom uint8 = 1
		sideTo   uint8 = 2
	)
	fc, err := r.CommitObject(from)
	if err != nil {
		return submoduleDelta{}, false, false
	}
	tc, err := r.CommitObject(to)
	if err != nil {
		return submoduleDelta{}, false, false
	}
	type visit struct {
		c    *object.Commit
		side uint8
	}
	q := []visit{{fc, sideFrom}, {tc, sideTo}}
	marks := make(map[plumbing.Hash]uint8, 64)
	var visited int
	for len(q) > 0 {
		v := q[0]
		q = q[1:]
		h := v.c.ID()
		if marks[h]&v.side != 0 {
			continue // this side already walked the commit
		}
		marks[h] |= v.side
		visited++
		if visited > gitSubmoduleWalkMax {
			capped = true
			break
		}
		if marks[h] == sideFrom|sideTo {
			continue // a common ancestor: everything past it is both sides'
		}
		if v.side == sideFrom {
			d.oldOnly = append(d.oldOnly, v.c)
		} else {
			d.newOnly = append(d.newOnly, v.c)
		}
		for _, p := range v.c.ParentHashes {
			pc, err := r.CommitObject(p)
			if err != nil {
				return submoduleDelta{}, false, false
			}
			q = append(q, visit{pc, v.side})
		}
	}
	// a wave reaching the other tip names the relation; the other side may
	// upgrade a listed commit to common afterwards, so filter at the end
	both := sideFrom | sideTo
	d.fastForward = !capped && marks[from] == both
	d.rewind = !capped && marks[to] == both
	newest := func(a, b *object.Commit) int { return b.Author.When.Compare(a.Author.When) }
	single := func(list []*object.Commit, side uint8) []*object.Commit {
		return bulk.SliceFilter(func(c *object.Commit) bool {
			return marks[c.ID()] == side
		}, list)
	}
	d.oldOnly = single(d.oldOnly, sideFrom)
	d.newOnly = single(d.newOnly, sideTo)
	slices.SortStableFunc(d.oldOnly, newest)
	slices.SortStableFunc(d.newOnly, newest)
	return d, capped, true
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
	default:
		head = append(head, "diff --git a/"+from.path+" b/"+to.path)
		// git prints the mode pair before the rename pair
		if from.mode != to.mode {
			head = append(head, "old mode "+gitMode(from.mode), "new mode "+gitMode(to.mode))
		}
		if from.path != to.path {
			head = append(head, "rename from "+from.path, "rename to "+to.path)
		}
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
func gitCommitDiffText(ctx context.Context, c *object.Commit, file string, sub submoduleRender, d *gitDiffOut) error {
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
	if err := gitDiffTreeText(ctx, fromTree, toTree, file, sub, d); err != nil {
		return err
	}
	if c.NumParents() > 1 {
		d.Meta("merge commit; showing the diff against first parent " + shortHash(parent.ID()))
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
// names HEAD's paths, files and gitlinks: status omits clean entries, so they
// must come separately.
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

// fixupGitlinkStatus corrects the submodule-blind entries go-git's status
// produces when a checkout is not declared in .gitmodules: contents of a
// gitlink still staged in the index are never loose untracked files, and a
// checkout on disk is pointer state, not a work-tree deletion. idxLinks
// carries the staged pointer each gitlink must match.
func fixupGitlinkStatus(root string, st git.Status, idxLinks map[string]plumbing.Hash) {
	for path, stagedHash := range idxLinks {
		checkedOut := gitCheckoutHash(root, path)
		if checkedOut.IsZero() {
			continue // no checkout there: go-git's own view stands
		}
		maps.DeleteFunc(st, func(p string, _ *git.FileStatus) bool {
			return p != path && strings.HasPrefix(p, path+"/")
		})
		if fs, ok := st[path]; ok && fs.Worktree == git.Deleted {
			// the checkout exists: report its pointer, not a deletion
			if checkedOut == stagedHash {
				fs.Worktree = git.Unmodified
			} else {
				fs.Worktree = git.Modified
			}
		}
	}
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
	out := make([]string, 0, len(untracked))
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

// gitTreeMaxDepth bounds subtree recursion the way go-git's own walker does,
// so hostile tree data cannot nest without end.
const gitTreeMaxDepth = 1024

// walkTreePaths calls fn with every non-directory entry under t as a full
// repo path, fetching each subtree through s. go-git's walkers fetch a blob
// per path and read a failed subtree fetch as end-of-iteration, which breaks
// blob-filtered clones and silently truncates listings. Entry names arrive
// raw from tree data: names go-git rejects as unsafe to materialise are
// legal in git, and these names are only compared, never handed to the
// filesystem.
func walkTreePaths(s storer.EncodedObjectStorer, t *object.Tree, fn func(path string, e object.TreeEntry) error) error {
	var walk func(t *object.Tree, prefix string, depth int) error
	walk = func(t *object.Tree, prefix string, depth int) error {
		if depth > gitTreeMaxDepth {
			return fmt.Errorf("%w at %d levels", object.ErrMaxTreeDepth, gitTreeMaxDepth)
		}
		for i := range t.Entries {
			e := t.Entries[i]
			name := e.Name
			if prefix != "" {
				name = prefix + "/" + e.Name
			}
			if e.Mode != filemode.Dir {
				if err := fn(name, e); err != nil {
					return err
				}
				continue
			}
			sub, err := object.GetTree(s, e.Hash)
			if err != nil {
				return fmt.Errorf("subtree %s: %w", name, err)
			}
			if err := walk(sub, name, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(t, "", 0)
}

// headTreePaths lists every path in HEAD's tree, files and gitlinks returned
// separately, both empty before any commit. A broken tree errors rather than
// reading as absent files.
func headTreePaths(r *git.Repository) (files, gitlinks []string, err error) {
	c, err := gitCommit(r, "HEAD")
	if err != nil {
		if errors.Is(err, errNoCommits) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	t, err := c.Tree()
	if err != nil {
		return nil, nil, err
	}
	err = walkTreePaths(r.Storer, t, func(path string, e object.TreeEntry) error {
		if e.Mode == filemode.Submodule {
			gitlinks = append(gitlinks, path)
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return files, gitlinks, nil
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
	files, gitlinks, err := headTreePaths(r)
	if err != nil {
		return "", err
	}
	idxLinks, err := indexGitlinks(r)
	if err != nil {
		return "", err
	}
	fixupGitlinkStatus(w.Filesystem.Root(), st, idxLinks)
	lines, staged, unstaged, untracked := gitStatusLines(st, append(files, gitlinks...))

	var b strings.Builder
	b.WriteString(gitHeadLine(r) + "\n")
	counts := make([]string, 0, 3)
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
