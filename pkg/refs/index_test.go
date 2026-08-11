package refs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCandidatesListTopLevel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTree(t, dir, "main.go", "src/main.go")
	idx := NewIndex(dir, tools.PathPolicy{})

	cands := idx.Candidates("", nil)
	assert.Equal(t, []string{"main.go", "src/"}, labelsOf(cands),
		"an empty query offers top-level files and directories only")
}

func TestCandidatesDirectoryDrillsDeeper(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTree(t, dir, "src/main.go", "src/cmd/run.go")
	idx := NewIndex(dir, tools.PathPolicy{})

	cands := idx.Candidates("src/", nil)
	assert.Equal(t, []string{"src/cmd/", "src/main.go"}, labelsOf(cands),
		"a trailing slash lists the directory's children with dirs marked")

	files := idx.Candidates("src/m", nil)
	assert.Equal(t, []string{"src/main.go"}, labelsOf(files))
}

func TestCandidatesPartialDirNameCompletesSlash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTree(t, dir, "pkg/refs/index.go")
	idx := NewIndex(dir, tools.PathPolicy{})

	cands := idx.Candidates("pk", nil)
	assert.Equal(t, []string{"pkg/"}, labelsOf(cands))

	sub := idx.Candidates("pkg/r", nil)
	assert.Equal(t, []string{"pkg/refs/"}, labelsOf(sub))
}

func TestCandidatesExcludesSkippedDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTree(t, dir, "main.go")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o700))
	idx := NewIndex(dir, tools.PathPolicy{})

	for _, q := range []string{"", "no"} {
		assert.NotContains(t, labelsOf(idx.Candidates(q, nil)), "node_modules/")
	}
}

func TestCandidatesConversationRankedFirst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTree(t, dir, "a.go", "b.go")
	idx := NewIndex(dir, tools.PathPolicy{})

	inConvo := func(path string) bool { return filepath.Base(path) == "b.go" }
	cands := idx.Candidates("", inConvo)
	assert.Equal(t, []string{"b.go", "a.go"}, labelsOf(cands),
		"an already-referenced file outranks a newer sibling")
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

// labelsOf returns each completion's label in order.
func labelsOf(cands []tui.Completion) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Label)
	}
	return out
}
