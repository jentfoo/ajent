//go:build unix

package tools

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// mkfifo creates the named pipe at path: the special file whose open blocks
// forever without a writer.
func mkfifo(t *testing.T, path string) {
	t.Helper()

	require.NoError(t, syscall.Mkfifo(path, 0o644))
}

// fifoFence runs fn, failing fast when it blocks instead of returning: every
// test here pins "the fifo is never opened", so a regression would otherwise
// hang the whole test binary.
func fifoFence(t *testing.T, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "blocked on a fifo")
}

// fifoErr is the refusal every guarded site reports for a special file.
var fifoErr = "not a regular file"

func TestRefuseSpecial(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "pipe")
	mkfifo(t, p)

	err := refuseSpecial(p)
	require.Error(t, err)
	assert.Equal(t, fifoErr, errors.Unwrap(err).Error())
	var pathErr *fs.PathError
	require.ErrorAs(t, err, &pathErr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "reg.txt"), []byte("x"), 0o644))
	assert.NoError(t, refuseSpecial(filepath.Join(dir, "reg.txt")))
	assert.NoError(t, refuseSpecial(filepath.Join(dir, "missing.txt")))
}

func TestSpecialFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"regular", 0o644, false},
		{"dir", os.ModeDir | 0o755, false},
		{"symlink", os.ModeSymlink, false},
		{"fifo", os.ModeNamedPipe, true},
		{"block_device", os.ModeDevice | 0o660, true},
		{"char_device", os.ModeDevice | os.ModeCharDevice | 0o644, true},
		{"socket", os.ModeSocket, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, specialFile(tc.mode))
		})
	}
}

func TestReadRefusesFifo(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	mkfifo(t, filepath.Join(e.cwd, "pipe.txt"))

	fifoFence(t, func() {
		res := e.readExec(t.Context(), `{"path":"pipe.txt"}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), fifoErr)
	})
}

func TestMeasureSkipsFifo(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "pipe")
	mkfifo(t, p)

	fifoFence(t, func() {
		m, err := Measure(p)
		require.NoError(t, err)
		assert.False(t, m.Dir)
		assert.Equal(t, KindSpecial, m.Kind) // annotated, never planned as a read
	})
}

// TestRefuseSpecialSocket drives the guard with a real socket node, the one
// special kind a fifo fixture cannot stand in for.
func TestRefuseSpecialSocket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "sock")
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "unix", p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	fifoFence(t, func() {
		err := refuseSpecial(p)
		require.Error(t, err)
		assert.Equal(t, fifoErr, errors.Unwrap(err).Error())

		m, err := Measure(p)
		require.NoError(t, err)
		assert.Equal(t, KindSpecial, m.Kind)
	})
}

// TestRefuseSpecialSymlink pins the intended symlink semantics: stat follows
// the link, so a link to a regular file reads while a link to a fifo refuses.
func TestRefuseSpecialSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "pipe"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "reg.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink("pipe", filepath.Join(dir, "link-fifo")))
	require.NoError(t, os.Symlink("reg.txt", filepath.Join(dir, "link-reg")))

	fifoFence(t, func() {
		require.Error(t, refuseSpecial(filepath.Join(dir, "link-fifo")))
		assert.NoError(t, refuseSpecial(filepath.Join(dir, "link-reg")))
	})
}

func TestEditRefusesFifo(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	mkfifo(t, filepath.Join(e.cwd, "pipe.txt"))
	args := `{"path":"pipe.txt","edits":[{"oldText":"a","newText":"b"}]}`

	fifoFence(t, func() {
		require.Error(t, e.editDryRun(args))
		res := e.editExec(t.Context(), args)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), fifoErr)
	})
}

func TestWriteDisplacedFifo(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "pipe")
	mkfifo(t, p)

	fifoFence(t, func() {
		d := readDisplaced(p)
		assert.Equal(t, dispUnreadable, d.state)
	})
}

// TestWriteRefusesFifo pins the execute-side refusal: write must not unlink a
// node it refuses to read, and the fifo survives the failed call.
func TestWriteRefusesFifo(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "pipe.txt")
	mkfifo(t, p)

	fifoFence(t, func() {
		res := e.writeExec(t.Context(), `{"path":"pipe.txt","content":"new\n"}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), fifoErr)

		fi, err := os.Lstat(p)
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeNamedPipe) // the node survives the refusal
	})
}

func TestWritePreviewRefusesFifo(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	mkfifo(t, filepath.Join(e.cwd, "pipe.txt"))
	c := agent.ToolCall{ID: "c", Name: "write",
		Input: json.RawMessage(`{"path":"pipe.txt","content":"new\n"}`)}

	fifoFence(t, func() {
		_, err := (&writeTool{policy: e.policy}).Preview(c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fifoErr)
	})
}

// TestWriteRefusesDevice drives the refusal with a real device node, which a
// fifo fixture cannot stand in for. Root would unlink it, so pin the refusal.
func TestWriteRefusesDevice(t *testing.T) {
	t.Parallel()

	fi, err := os.Stat("/dev/null")
	if err != nil || fi.Mode()&os.ModeDevice == 0 {
		t.Skip("no device node at /dev/null")
	}

	e := newToolEnv(t.TempDir())
	fifoFence(t, func() {
		res := e.writeExec(t.Context(), `{"path":"/dev/null","content":"x"}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), fifoErr)
	})
}

func TestTrackerUnchangedSkipsFifo(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "body\n")
	assert.False(t, e.readExec(t.Context(), `{"path":"a.txt"}`).IsError)
	require.NoError(t, os.Remove(filepath.Join(e.cwd, "a.txt")))
	mkfifo(t, filepath.Join(e.cwd, "a.txt"))

	fifoFence(t, func() {
		assert.False(t, e.tracker.Unchanged(filepath.Join(e.cwd, "a.txt")))
	})
}

func TestGitDiskSideFifo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "pipe.txt"))

	fifoFence(t, func() {
		side, isDir := gitDiskSide(dir, "pipe.txt")
		assert.Nil(t, side)
		assert.False(t, isDir)
	})
}

// TestGitDiffWorktreeFifo replays the todo's repro: a tracked file replaced by
// an unopened fifo must render as a deletion, not hang the diff.
func TestGitDiffWorktreeFifo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pipe.txt"), []byte("tracked\n"), 0o644))
	_, err = w.Add("pipe.txt")
	require.NoError(t, err)
	_, err = w.Commit("add pipe", &git.CommitOptions{Author: gitAuthor(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "pipe.txt")))
	mkfifo(t, filepath.Join(dir, "pipe.txt"))

	fifoFence(t, func() {
		d := newGitDiffOut("fifo-test", "git_diff")
		defer func() { _ = d.close() }()
		require.NoError(t, gitDiffText(t.Context(), r, "", "worktree", "", submoduleRender{}, d))
		out := d.Text("")
		assert.Contains(t, out, "diff --git a/pipe.txt b/pipe.txt")
		assert.Contains(t, out, "deleted file mode")
	})
}

// TestFindDotGitSkipsFifo pins the hostile-tree variant: a fifo planted at a
// .git pointer path must read as "not a repository", never hang repo open.
func TestFindDotGitSkipsFifo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, ".git"))

	fifoFence(t, func() {
		dotRoot, wtRoot := findDotGit(filepath.Join(dir, "nested"))
		assert.Empty(t, dotRoot)
		assert.Empty(t, wtRoot)
	})
}

// TestGuardedFS pins the wrapper go-git's own opens go through: a regular file
// reads, a fifo and a symlink to one refuse, a symlink to a regular file reads.
func TestGuardedFS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))
	mkfifo(t, filepath.Join(dir, "pipe"))
	require.NoError(t, os.Symlink("pipe", filepath.Join(dir, "link-fifo")))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(dir, "link-reg")))
	fs := guardedFS{osfs.New(dir)}

	f, err := fs.Open("a.txt")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	lf, err := fs.Open("link-reg")
	require.NoError(t, err)
	require.NoError(t, lf.Close())

	fifoFence(t, func() {
		for _, name := range []string{"pipe", "link-fifo"} {
			_, err := fs.Open(name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), fifoErr)
		}
	})
}

// TestGitStatusRefusesFifoHead pins the storage-side guard: go-git opens HEAD
// itself, so a fifo planted there must error rather than hang repo reading.
// go-git re-maps the refusal to its own not-a-repository wording.
func TestGitStatusRefusesFifoHead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, ".git", "HEAD")))
	mkfifo(t, filepath.Join(dir, ".git", "HEAD"))

	fifoFence(t, func() {
		out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: dir}}, `{}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "not a git repository")
	})
}

// TestGitStatusRefusesFifoGitmodules pins the worktree-side guard: status opens
// .gitmodules itself through the worktree filesystem go-git was handed.
func TestGitStatusRefusesFifoGitmodules(t *testing.T) {
	t.Parallel()

	g := newGitRepo(t)
	mkfifo(t, filepath.Join(g.dir, ".gitmodules"))

	fifoFence(t, func() {
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.True(t, isErr)
		assert.Contains(t, out, fifoErr)
	})
}

// TestGitShowRefusesFifoObject pins the object-store guard: a loose object read
// must refuse a fifo planted in its place. Content carries the test name so the
// commit hash is unique and the process-wide object cache cannot serve it.
func TestGitShowRefusesFifoObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"),
		[]byte("content "+t.Name()+"\n"), 0o644))
	_, err = w.Add("a.txt")
	require.NoError(t, err)
	head, err := w.Commit("add a", &git.CommitOptions{Author: gitAuthor(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))})
	require.NoError(t, err)

	s := head.String()
	obj := filepath.Join(dir, ".git", "objects", s[:2], s[2:])
	require.NoError(t, os.Remove(obj))
	mkfifo(t, obj)

	// go-git re-maps the refused object read to its unborn-HEAD wording
	fifoFence(t, func() {
		out, isErr := runGitTool(t, &gitShowTool{policy: PathPolicy{Cwd: dir}}, `{}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "no commits")
	})
}
