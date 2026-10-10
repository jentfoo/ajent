package refs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/tui"
)

func TestHomeOneLevel(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, "deep/one/two/three.txt", ".bashrc")
	restoreHome(t, home)
	idx := NewIndex(t.TempDir())

	top := idx.Candidates("~", nil)
	assert.Equal(t, []string{"~/.bashrc", "~/deep/"}, textsOf(top)) // deep once, never its contents
	assert.Equal(t, []string{".bashrc", "deep/"}, labelsOf(top))
}

func TestCandidates(t *testing.T) {
	t.Run("list_top_level", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "main.go", "src/main.go")
		idx := NewIndex(dir)

		cands := idx.Candidates("", nil)
		assert.Equal(t, []string{"main.go", "src/"}, labelsOf(cands))
	})

	t.Run("never_walks_the_tree", func(t *testing.T) {
		// a bare @ lists only the cwd's immediate children: deep files must not
		// surface until the user drills into their directory.
		dir := t.TempDir()
		writeTree(t, dir, "main.go", "deep/one/two/three.txt")
		idx := NewIndex(dir)

		assert.Equal(t, []string{"deep/", "main.go"}, labelsOf(idx.Candidates("", nil)))
		// drilling one level at a time reaches the deep file, inserted in full
		sub := idx.Candidates("deep/one/two/th", nil)
		assert.Equal(t, []string{"deep/one/two/three.txt"}, textsOf(sub))
		assert.Equal(t, []string{"three.txt"}, labelsOf(sub))
	})

	t.Run("directory_drills_deeper", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "src/main.go", "src/cmd/run.go")
		idx := NewIndex(dir)

		cands := idx.Candidates("src/", nil)
		assert.Equal(t, []string{"src/cmd/", "src/main.go"}, textsOf(cands))
		assert.Equal(t, []string{"cmd/", "main.go"}, labelsOf(cands))

		files := idx.Candidates("src/m", nil)
		assert.Equal(t, []string{"src/main.go"}, textsOf(files))
	})

	t.Run("listing_shows_bare_names", func(t *testing.T) {
		// the listing packs columns of entry names; only the accepted text is a path
		dir := t.TempDir()
		writeTree(t, dir, "pkg/refs/a_very_long_file_name_indeed.go")
		idx := NewIndex(dir)

		cands := idx.Candidates("pkg/re", nil)
		assert.Equal(t, []string{"pkg/refs/"}, textsOf(cands))
		assert.Equal(t, []string{"refs/"}, labelsOf(cands))
	})

	t.Run("partial_dir_completes_slash", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/refs/index.go")
		idx := NewIndex(dir)

		cands := idx.Candidates("pk", nil)
		assert.Equal(t, []string{"pkg/"}, labelsOf(cands))

		sub := idx.Candidates("pkg/r", nil)
		assert.Equal(t, []string{"pkg/refs/"}, textsOf(sub))
	})

	t.Run("dot_prefix_keeps_slash", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "main.go", "src/main.go", "src/cmd/run.go")
		idx := NewIndex(dir)

		top := idx.Candidates("./", nil)
		assert.Equal(t, []string{"./main.go", "./src/"}, textsOf(top)) // the inserted text keeps ./
		assert.Equal(t, []string{"main.go", "src/"}, labelsOf(top))

		drill := idx.Candidates("./src/m", nil)
		assert.Equal(t, []string{"./src/main.go"}, textsOf(drill))
	})

	t.Run("absolute_path_keeps_root_slash", func(t *testing.T) {
		dir := t.TempDir() // workspace root, a sibling lives alongside it
		writeTree(t, dir, "main.go")
		parent := filepath.Dir(dir)
		baseName := filepath.Base(dir)
		idx := NewIndex(dir)

		drill := idx.Candidates(filepath.Join(dir, "ma"), nil)
		assert.Equal(t, []string{filepath.Join(dir, "main.go")}, textsOf(drill))

		// a trailing slash lists that directory's immediate children
		children := idx.Candidates(dir+"/", nil)
		assert.Equal(t, []string{dir + "/main.go"}, textsOf(children))

		// the parent dir shows the workspace as one completable sibling
		siblings := textsOf(idx.Candidates(parent+"/", nil))
		require.True(t, slices.ContainsFunc(siblings,
			func(l string) bool { return strings.HasSuffix(l, baseName+"/") }))
	})

	t.Run("excludes_skipped_dirs", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "main.go")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o700))
		idx := NewIndex(dir)

		for _, q := range []string{"", "no"} {
			assert.NotContains(t, labelsOf(idx.Candidates(q, nil)), "node_modules/")
		}
	})

	t.Run("conversation_ranked_first", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "a.go", "b.go")
		idx := NewIndex(dir)

		inConvo := func(path string) bool { return filepath.Base(path) == "b.go" }
		cands := idx.Candidates("", inConvo)
		assert.Equal(t, []string{"b.go", "a.go"}, labelsOf(cands))
	})

	// tilde completion mutates the global userHome, so it stays sequential and
	// shares one root + home tree across its cases.
	t.Run("tilde", func(t *testing.T) {
		root := t.TempDir() // workspace, must not leak into home completion
		home := t.TempDir()
		writeTree(t, root, "main.go")
		writeTree(t, home, ".bashrc", "docs/a.txt", "pkg/main.go", "bin/run.sh")
		restoreHome(t, home)
		idx := NewIndex(root)

		t.Run("offers_home_top_level_only", func(t *testing.T) {
			cands := idx.Candidates("~", nil)
			for _, c := range cands {
				assert.True(t, strings.HasPrefix(c.Text, "~/"))
			}
			assert.NotContains(t, labelsOf(cands), "main.go")
		})

		t.Run("drills_deeper_with_slash", func(t *testing.T) {
			docs := idx.Candidates("~/d", nil)
			require.Equal(t, []string{"~/docs/"}, textsOf(docs))

			pk := idx.Candidates("~/pkg/m", nil)
			assert.Equal(t, []string{"~/pkg/main.go"}, textsOf(pk))

			bin := idx.Candidates("~/bin/", nil)
			assert.Equal(t, []string{"~/bin/run.sh"}, textsOf(bin))
		})
	})
}

func TestShellCandidates(t *testing.T) {
	t.Parallel()

	// a shell command may name a VCS or dependency directory that @ hides
	t.Run("offers_skipped_dirs", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "main.go", "node_modules/pkg/index.js", ".git/config")
		idx := NewIndex(dir)

		assert.Equal(t, []string{"main.go"}, labelsOf(idx.Candidates("", nil)))
		shell := labelsOf(idx.ShellCandidates(""))
		assert.ElementsMatch(t, []string{".git/", "main.go", "node_modules/"}, shell)

		assert.Equal(t, []string{".git/config"}, textsOf(idx.ShellCandidates(".git/")))
	})

	// a truncated set would yield too long a common prefix, skipping a branch
	t.Run("returns_every_match", func(t *testing.T) {
		dir := t.TempDir()
		names := make([]string, 0, 100)
		for i := range 100 {
			names = append(names, fmt.Sprintf("file%03d.txt", i))
		}
		writeTree(t, dir, names...)
		idx := NewIndex(dir)

		assert.Len(t, idx.ShellCandidates("file"), 100)
		assert.Len(t, idx.Candidates("file", nil), 100)
	})
}

// restoreHome points the injected userHome at home for the duration of a test.
func restoreHome(t *testing.T, home string) {
	t.Helper()

	orig := userHome
	userHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHome = orig })
}

// writeTree creates the given relative files (creating parent dirs) under root.
func writeTree(t *testing.T, root string, rels ...string) {
	t.Helper()

	for _, rel := range rels {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
}

// labelsOf returns each completion's listing label in order.
func labelsOf(cands []tui.Completion) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Label)
	}
	return out
}

// textsOf returns each completion's inserted text in order.
func textsOf(cands []tui.Completion) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Text)
	}
	return out
}
