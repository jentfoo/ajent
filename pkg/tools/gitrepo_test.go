package tools

import (
	"os"
	"path/filepath"
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

// submoduleRepo is a parent repo whose "sub" path is a gitlink into a child
// repository, set through the index alone: no checkout and no .gitmodules, so
// the pointer is the only submodule state that exists.
//
//	pre (a.txt) --- base (sub @ child1) --- bump (sub @ child2)
type submoduleRepo struct {
	dir             string
	pre, base, bump string // parent commits around the pointer moves
	child1          string // child commit the base pointer names
}

// newSubmoduleRepo builds the fixture in temp directories.
func newSubmoduleRepo(t *testing.T) *submoduleRepo {
	t.Helper()

	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// child repo supplies the two commits the pointer moves between
	child := t.TempDir()
	cr, err := git.PlainInit(child, false)
	require.NoError(t, err)
	cw, err := cr.Worktree()
	require.NoError(t, err)
	childHead := func(content string) string {
		t.Helper()
		require.NoError(t, os0Write(child, "lib.txt", content))
		_, err = cw.Add(".")
		require.NoError(t, err)
		h, err := cw.Commit("child "+content, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		return h.String()
	}
	child1 := childHead("one\n")
	child2 := childHead("two\n")

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	setGitlink := func(hash string) {
		t.Helper()
		idx, err := r.Storer.Index()
		require.NoError(t, err)
		var seen bool
		for _, e := range idx.Entries {
			if e.Name == "sub" {
				e.Hash = plumbing.NewHash(hash)
				seen = true
			}
		}
		if !seen {
			idx.Entries = append(idx.Entries, &index.Entry{
				Name: "sub", Hash: plumbing.NewHash(hash), Mode: filemode.Submodule,
			})
		}
		require.NoError(t, r.Storer.SetIndex(idx))
	}
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
	return &submoduleRepo{dir: dir, pre: pre, base: base, bump: bump, child1: child1}
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

// policy returns a PathPolicy rooted at the repo.
func (g *submoduleRepo) policy() PathPolicy {
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

// os0Write writes name under dir, creating parent directories.
func os0Write(dir, name, content string) error {
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
