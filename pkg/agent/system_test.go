package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSystem(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"}

	blocks := buildSystem(s, env, nil, nil)
	assert.Len(t, blocks, 1)

	tb, ok := blocks[0].(llm.TextBlock)
	require.True(t, ok) // a single text block is the whole system prompt
	for _, want := range []string{"/repo", "linux/amd64", "2024-01-02"} {
		assert.Contains(t, tb.Text, want)
	}
}

// TestIdentityLine pins the domain-neutral opening sentence so a wording change is
// deliberate and reviewed.
func TestIdentityLine(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "You help by following the user's instructions: research and review until you understand them, then focus on what is asked.\n\n", identityLine())
}

// TestBuildSystemCacheStable asserts equal inputs produce byte-identical blocks,
// so the provider prompt cache survives between requests.
func TestBuildSystemCacheStable(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/r", OS: "linux", Date: "2024-01-02 09:00"}

	b1, ok := buildSystem(s, env, nil, nil)[0].(llm.TextBlock)
	require.True(t, ok)
	b2, ok := buildSystem(s, env, nil, nil)[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, b1.Text, b2.Text)
}

// TestBuildSystemDateDayGranular asserts the date changes at day granularity,
// not sub-day.
func TestBuildSystemDateDayGranular(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	b1, ok := buildSystem(s, Environment{Cwd: "/r", OS: "linux", Date: "2024-01-02 09:00"}, nil, nil)[0].(llm.TextBlock)
	require.True(t, ok)
	b2, ok := buildSystem(s, Environment{Cwd: "/r", OS: "linux", Date: "2024-01-03 08:59"}, nil, nil)[0].(llm.TextBlock)
	require.True(t, ok)

	assert.NotEqual(t, b1.Text, b2.Text) // the date differs across days
}

// TestBuildSystemListsCwd asserts the ls-style directory listing is emitted.
func TestBuildSystemListsCwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("// x\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))

	s := &State{Model: llm.Model{ID: "test"}}
	blocks := buildSystem(s, Environment{Cwd: dir}, nil, nil)
	tb, ok := blocks[0].(llm.TextBlock)
	require.True(t, ok)

	assert.Contains(t, tb.Text, "Directory contents:")
	assert.Contains(t, tb.Text, "  a.go\n")
	assert.Contains(t, tb.Text, "  sub/\n")
}

// TestBuildSystemOmitsMissingCwdListing tolerates an absent cwd.
func TestBuildSystemOmitsMissingCwdListing(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	blocks := buildSystem(s, Environment{Cwd: "/does/not/exist"}, nil, nil)
	tb, ok := blocks[0].(llm.TextBlock)
	require.True(t, ok)

	assert.Contains(t, tb.Text, "Working directory: /does/not/exist\n")
	assert.NotContains(t, tb.Text, "Directory contents:")
}

// TestBuildSystemSnippetsAppendAfterProject asserts snippets land after project
// instructions, each separated by a blank line.
func TestBuildSystemSnippetsAppendAfterProject(t *testing.T) {
	t.Parallel()

	base := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/repo", Date: "2024-01-02"}

	empty, ok := buildSystem(base, env, nil, nil)[0].(llm.TextBlock)
	require.True(t, ok)
	withProj, ok := buildSystem(base, env,
		[]ProjectInstruction{{Path: "/repo/AGENTS.md", Body: "# Rules\n"}}, nil)[0].(llm.TextBlock)
	require.True(t, ok)

	snippets, ok := buildSystem(base, env,
		[]ProjectInstruction{{Path: "/repo/AGENTS.md", Body: "# Rules\n"}},
		[]string{"first snippet", "second snippet"})[0].(llm.TextBlock)
	require.True(t, ok)

	snippetsOnly, ok := buildSystem(base, env, nil, []string{})[0].(llm.TextBlock)
	require.True(t, ok)
	// empty snippets must not change the block at all.
	assert.Equal(t, empty.Text, snippetsOnly.Text)
	assert.NotEqual(t, empty.Text, withProj.Text) // proj alone still differs

	snippetsOnlyText := buildSystem(base, env, nil, []string{"first snippet"})[0].(llm.TextBlock)
	// a snippet appended without project instructions is separated by a blank line.
	assert.Contains(t, snippetsOnlyText.Text, "\n\nfirst snippet\n")

	// each snippet appears after the project context and is newline-separated.
	i1 := strings.Index(snippets.Text, "</project_context>")
	i2 := strings.Index(snippets.Text, "first snippet")
	i3 := strings.Index(snippets.Text, "second snippet")
	assert.Greater(t, i2, i1)
	assert.Greater(t, i3, i2)
}

// TestDetectEnvironment exercises the real machine-fact probes.
func TestDetectEnvironment(t *testing.T) {
	t.Parallel()

	env := DetectEnvironment()
	assert.NotEmpty(t, env.Cwd)
	assert.NotEmpty(t, env.OS)
	assert.NotEmpty(t, env.Date)
}

// TestListCwd sorts names and marks directories.
func TestListCwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("// x\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a"), 0o755))

	assert.Equal(t, []string{"a/", "b.go"}, listCwd(dir))
}

// TestListCwdMissing returns nil for an absent dir.
func TestListCwdMissing(t *testing.T) {
	t.Parallel()

	assert.Nil(t, listCwd("/does/not/exist"))
}

// TestBuildSystemProjectInstructions asserts the provenance-marked <project_context>
// block is appended after environment facts when instructions are present.
func TestBuildSystemProjectInstructions(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/repo", Date: "2024-01-02"}
	proj := []ProjectInstruction{{Path: "/repo/AGENTS.md", Body: "# Rules\nbuild with make test\n"}}

	blocks := buildSystem(s, env, proj, nil)
	tb, ok := blocks[0].(llm.TextBlock)
	require.True(t, ok)

	for _, want := range []string{
		"<project_context>",
		"Project-specific instructions and guidelines:",
		`<project_instructions path="/repo/AGENTS.md">`,
		"# Rules",
		"build with make test",
		"</project_instructions>",
		"</project_context>",
	} {
		assert.Contains(t, tb.Text, want)
	}

	// provenance block comes after the environment facts, not before them
	assert.Less(t,
		strings.Index(tb.Text, "Working directory: /repo\n"),
		strings.Index(tb.Text, "<project_context>"),
		"project instructions must follow environment facts")
}

// TestLoadProjectInstructions reads AGENTS.md from a dir when present.
func TestLoadProjectInstructions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Rules\n"), 0o644))

	proj, err := LoadProjectInstructions(dir)
	require.NoError(t, err)
	assert.Len(t, proj, 1)
	assert.Equal(t, filepath.Join(dir, "AGENTS.md"), proj[0].Path)
	assert.Equal(t, "# Rules\n", proj[0].Body)
}

// TestLoadProjectInstructionsMissing returns nil when no AGENTS.md exists.
func TestLoadProjectInstructionsMissing(t *testing.T) {
	t.Parallel()

	proj, err := LoadProjectInstructions(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, proj)
}

// TestLoadProjectInstructionsLayers orders global before project and skips empty dirs.
func TestLoadProjectInstructionsLayers(t *testing.T) {
	t.Parallel()

	global := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("# Global\n"), 0o644))
	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("# Project\n"), 0o644))

	proj, err := LoadProjectInstructions(global, project)
	require.NoError(t, err)
	require.Len(t, proj, 2)
	assert.Equal(t, filepath.Join(global, "AGENTS.md"), proj[0].Path)
	assert.Equal(t, filepath.Join(project, "AGENTS.md"), proj[1].Path)
}

// TestLoadProjectInstructionsSkipsEmptyAndAbsent tolerates a missing home and an absent cwd.
func TestLoadProjectInstructionsSkipsEmptyAndAbsent(t *testing.T) {
	t.Parallel()

	proj, err := LoadProjectInstructions("", t.TempDir(), "")
	require.NoError(t, err)
	assert.Nil(t, proj)
}
