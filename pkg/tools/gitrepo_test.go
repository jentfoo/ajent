package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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

// policy returns a PathPolicy rooted at the repo, plus the session spill id.
func (g *gitRepo) policy() PathPolicy {
	return PathPolicy{Cwd: g.dir}
}

// write drops a work-tree file without committing it.
func (g *gitRepo) write(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os0Write(g.dir, name, content))
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
