package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// gitRepo is a real repository built with go-git itself, so fixtures are the
// exact bytes the tools parse and no ambient system git takes part.
//
//	main   A --- M (merge of B)
//	        \   /
//	side     B
//
// A adds a.txt and bin.dat, B edits a.txt on the side branch, M merges B with
// explicit parents. Tag v1 is annotated on B.
type gitRepo struct {
	dir     string
	a, b, m string // commit hashes
}

// gitAuthor pins signatures so output never depends on the clock.
func gitAuthor(when time.Time) *object.Signature {
	return &object.Signature{Name: "Ajent Test", Email: "test@ajent.local", When: when}
}

// newGitRepo builds the fixture in a temp directory.
func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	write := func(name, content string) {
		t.Helper()

		require.NoError(t, os0Write(dir, name, content))
	}
	commit := func(msg string, when time.Time, parents ...string) string {
		t.Helper()

		_, err := w.Add(".")
		require.NoError(t, err)
		opts := &git.CommitOptions{Author: gitAuthor(when)}
		for _, p := range parents {
			opts.Parents = append(opts.Parents, plumbing.NewHash(p))
		}
		h, err := w.Commit(msg, opts)
		require.NoError(t, err)
		return h.String()
	}

	write("a.txt", "one\ntwo\nthree\n")
	write("bin.dat", "x\x00y\x00z")
	a := commit("add files", base)

	require.NoError(t, w.Checkout(&git.CheckoutOptions{Branch: "refs/heads/side", Create: true}))
	write("a.txt", "one\nTWO\nthree\n")
	b := commit("side edit of a.txt", base.Add(time.Minute))

	require.NoError(t, w.Checkout(&git.CheckoutOptions{Branch: "refs/heads/master"}))
	write("a.txt", "one\nTWO\nthree\n") // merged content on the merge commit
	m := commit("merge side", base.Add(2*time.Minute), a, b)

	_, err = r.CreateTag("v1", plumbing.NewHash(b), &git.CreateTagOptions{
		Tagger: gitAuthor(base.Add(time.Minute)), Message: "side edit",
	})
	require.NoError(t, err)

	return &gitRepo{dir: dir, a: a, b: b, m: m}
}

// childRepo supplies a two-commit child repository, the commits a submodule
// pointer moves between.
func childRepo(t *testing.T) (dir, first, second string) {
	t.Helper()

	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	dir = t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	head := func(content string) string {
		t.Helper()
		require.NoError(t, os0Write(dir, "lib.txt", content))
		_, err = w.Add(".")
		require.NoError(t, err)
		h, err := w.Commit("child "+content, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		return h.String()
	}
	return dir, head("one\n"), head("two\n")
}

// submoduleRepo is a parent repo whose "sub" path is a gitlink into a child
// repository, set through the index alone: no checkout and no .gitmodules, so
// the pointer is the only submodule state that exists.
//
//	pre (a.txt) --- base (sub @ child1) --- bump (sub @ child2)
//	                                         \--- filed (sub = blob) --- linked (sub @ child2)
type submoduleRepo struct {
	dir             string
	pre, base, bump string // parent commits around the pointer moves
	child1, child2  string // child commits the pointer moves between
	filed, linked   string // sub flipped to a blob, then back to a gitlink
}

// newSubmoduleRepo builds the fixture in temp directories.
func newSubmoduleRepo(t *testing.T) *submoduleRepo {
	t.Helper()

	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	_, child1, child2 := childRepo(t)

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	// setIndex stages one entry for the next commit without touching the work
	// tree; gitlinks name child commits no parent object needs to hold
	setIndex := func(name string, mode filemode.FileMode, hash plumbing.Hash) {
		t.Helper()
		idx, err := r.Storer.Index()
		require.NoError(t, err)
		var seen bool
		for _, e := range idx.Entries {
			if e.Name == name {
				e.Mode = mode
				e.Hash = hash
				seen = true
			}
		}
		if !seen {
			idx.Entries = append(idx.Entries, &index.Entry{Name: name, Mode: mode, Hash: hash})
		}
		require.NoError(t, r.Storer.SetIndex(idx))
	}
	setGitlink := func(hash string) { setIndex("sub", filemode.Submodule, plumbing.NewHash(hash)) }
	parentCommit := func(msg string) string {
		t.Helper()
		h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		when = when.Add(time.Minute)
		return h.String()
	}

	require.NoError(t, os0Write(dir, "a.txt", "one\n"))
	_, err = w.Add(".")
	require.NoError(t, err)
	pre := parentCommit("before submodule")
	setGitlink(child1)
	base := parentCommit("add submodule")
	setGitlink(child2)
	bump := parentCommit("bump submodule")

	// sub swaps to a plain blob and back: both directions of the mode flip
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	_, err = obj.Write([]byte("tracked\n"))
	require.NoError(t, err)
	blobHash, err := r.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	setIndex("sub", filemode.Regular, blobHash)
	filed := parentCommit("file over gitlink")
	setGitlink(child2)
	linked := parentCommit("gitlink back")
	return &submoduleRepo{
		dir: dir, pre: pre, base: base, bump: bump, child1: child1, child2: child2,
		filed: filed, linked: linked,
	}
}

// point restages the gitlink at hash without committing, an uncommitted
// pointer move.
func (g *submoduleRepo) point(t *testing.T, hash string) {
	t.Helper()
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	idx, err := r.Storer.Index()
	require.NoError(t, err)
	for _, e := range idx.Entries {
		if e.Name == "sub" {
			e.Hash = plumbing.NewHash(hash)
		}
	}
	require.NoError(t, r.Storer.SetIndex(idx))
}

// stageGitlink index-stages a new gitlink at path with no checkout and no
// .gitmodules, the shape a manual `update-index --add --cacheinfo` produces.
func (g *submoduleRepo) stageGitlink(t *testing.T, path, hash string) {
	t.Helper()
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	idx, err := r.Storer.Index()
	require.NoError(t, err)
	idx.Entries = append(idx.Entries, &index.Entry{
		Name: path, Hash: plumbing.NewHash(hash), Mode: filemode.Submodule,
	})
	require.NoError(t, r.Storer.SetIndex(idx))
}

// policy returns a PathPolicy rooted at the repo.
func (g *submoduleRepo) policy() PathPolicy {
	return PathPolicy{Cwd: g.dir}
}

// newSubmoduleSweepRepo commits n gitlinks at one child commit, then moves
// every pointer to another. The from..to diff names n submodule changes and
// nothing else.
func newSubmoduleSweepRepo(t *testing.T, n int) (dir, from, to string) {
	t.Helper()

	_, child1, child2 := childRepo(t)
	dir = t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	when := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	sweep := func(hash string) {
		t.Helper()
		idx, err := r.Storer.Index()
		require.NoError(t, err)
		byName := make(map[string]*index.Entry, len(idx.Entries))
		for _, e := range idx.Entries {
			byName[e.Name] = e
		}
		for i := range n {
			name := "sub" + strconv.Itoa(i)
			if e, ok := byName[name]; ok {
				e.Hash = plumbing.NewHash(hash)
				continue
			}
			idx.Entries = append(idx.Entries, &index.Entry{
				Name: name, Hash: plumbing.NewHash(hash), Mode: filemode.Submodule,
			})
		}
		require.NoError(t, r.Storer.SetIndex(idx))
	}
	commit := func(msg string) string {
		t.Helper()
		h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		when = when.Add(time.Minute)
		return h.String()
	}

	require.NoError(t, os0Write(dir, "a.txt", "one\n"))
	_, err = w.Add(".")
	require.NoError(t, err)
	sweep(child1)
	from = commit("sweep base")
	sweep(child2)
	to = commit("sweep bump")
	return dir, from, to
}

// checkoutRepo is a parent whose "sub" path is a real submodule checkout:
// .gitmodules committed, an initialized entry in .git/config and a clone on
// disk. Built with system git, since go-git cannot materialise submodules.
//
//	pre (a.txt) --- base (sub @ second)
type checkoutRepo struct {
	dir           string
	pre, base     string // parent commits before and with the submodule
	first, second string // child commits the checkout can move between
}

// newCheckoutRepo builds the fixture in temp directories.
func newCheckoutRepo(t *testing.T) *checkoutRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	child, first, second := childRepo(t)

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os0Write(dir, "a.txt", "one\n"))
	_, err = w.Add(".")
	require.NoError(t, err)
	when := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	commit := func(msg string) string {
		t.Helper()
		h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		when = when.Add(time.Minute)
		return h.String()
	}
	pre := commit("before submodule")
	gitRun(t, dir, "-c", "protocol.file.allow=always", "-c", "user.email=t@t", "-c", "user.name=t",
		"submodule", "add", "-q", child, "sub")
	_, err = w.Add(".") // stage .gitmodules beside the gitlink submodule add staged
	require.NoError(t, err)
	return &checkoutRepo{dir: dir, pre: pre, base: commit("submodule checkout"), first: first, second: second}
}

// checkout moves the submodule's checkout to hash.
func (g *checkoutRepo) checkout(t *testing.T, hash string) {
	t.Helper()
	gitRun(t, filepath.Join(g.dir, "sub"), "checkout", "-q", hash)
}

// policy returns a PathPolicy rooted at the repo.
func (g *checkoutRepo) policy() PathPolicy {
	return PathPolicy{Cwd: g.dir}
}

// policy returns a PathPolicy rooted at the repo, plus the session spill id.
func (g *gitRepo) policy() PathPolicy {
	return PathPolicy{Cwd: g.dir}
}

// write drops a work-tree file without committing it.
func (g *gitRepo) write(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os0Write(g.dir, name, content))
}

// commitAll stages every work-tree change and commits it, returning the hash.
func (g *gitRepo) commitAll(t *testing.T, msg string) string {
	t.Helper()
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	_, err = w.Add(".")
	require.NoError(t, err)
	h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC))})
	require.NoError(t, err)
	return h.String()
}

// renameChmodRepo commits old.sh at oldMode, then renames it to new.sh with
// newMode and content, returning the repo and the rename commit's hash.
func renameChmodRepo(t *testing.T, oldMode os.FileMode, content string, newMode os.FileMode) (*gitRepo, string) {
	t.Helper()
	g := newGitRepo(t)
	oldPath := filepath.Join(g.dir, "old.sh")
	newPath := filepath.Join(g.dir, "new.sh")
	require.NoError(t, os.WriteFile(oldPath, []byte("echo hi\n"), oldMode))
	g.commitAll(t, "add script")
	require.NoError(t, os.Rename(oldPath, newPath))
	require.NoError(t, os.WriteFile(newPath, []byte(content), newMode))
	require.NoError(t, os.Chmod(newPath, newMode)) // WriteFile keeps the mode of an existing file
	return g, g.commitAll(t, "rename script")
}

// stage stages every work-tree change, as git add -A would.
func (g *gitRepo) stage(t *testing.T) {
	t.Helper()
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	_, err = w.Add(".")
	require.NoError(t, err)
}

// headTreeEntry resolves one HEAD-tree entry's hash, for object surgery in
// tests. A directory name resolves to its subtree object's hash.
func headTreeEntry(t *testing.T, r *git.Repository, name string) plumbing.Hash {
	t.Helper()
	c, err := gitCommit(r, "")
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	e, err := tree.FindEntry(name)
	require.NoError(t, err)
	return e.Hash
}

// removeLoose deletes one loose object file, damaging the repository the way
// a corrupt object store would.
func removeLoose(t *testing.T, dir, hash string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(dir, ".git", "objects", hash[:2], hash[2:])))
}

// nestedGitRepo commits dir/nested.txt on top of the base fixture, the
// subtree shape the corrupt-object tests damage.
func nestedGitRepo(t *testing.T) (*gitRepo, *git.Repository) {
	t.Helper()
	g := newGitRepo(t)
	g.write(t, "dir/nested.txt", "nested\n")
	g.commitAll(t, "add nested")
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	return g, r
}

// os0Write writes name under dir, creating parent directories.
func os0Write(dir, name, content string) error {
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
