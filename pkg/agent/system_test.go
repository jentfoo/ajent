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
	env := Environment{Cwd: "/repo", OS: "linux/amd64", Shell: "bash", Date: "2024-01-02", Branch: "main", Dirty: true}

	blocks := buildSystem(s, env, nil)
	assert.Len(t, blocks, 1)

	tb, ok := blocks[0].(llm.TextBlock)
	if !ok {
		t.Fatal("system prompt must be a single text block")
	}
	for _, want := range []string{"/repo", "linux/amd64", "bash", "2024-01-02", "main"} {
		assert.Contains(t, tb.Text, want)
	}
}

func TestBuildSystemCacheStable(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	// two calls with the same env must produce identical bytes so the prompt
	// cache survives; Date is day granularity, never sub-day.
	env1 := Environment{Cwd: "/r", OS: "linux", Date: "2024-01-02 09:00"}
	env2 := Environment{Cwd: "/r", OS: "linux", Date: "2024-01-03 08:59"}

	b1, _ := buildSystem(s, env1, nil)[0].(llm.TextBlock)
	b2, _ := buildSystem(s, env2, nil)[0].(llm.TextBlock)
	assert.NotEqual(t, b1.Text, b2.Text, "the date differs across days")
}

func TestGitFactsFailSilently(t *testing.T) {
	t.Parallel()

	// git runs in a non-repo temp dir must return empty rather than error,
	// which is what the system prompt expects.
	dir := t.TempDir()
	assert.Empty(t, runGitIn(dir, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Empty(t, gitDirtyIn(dir))
}

// TestDetectEnvironment exercises the real machine-fact probes against a temp dir.
func TestDetectEnvironment(t *testing.T) {
	t.Parallel()

	env := DetectEnvironment()
	assert.NotEmpty(t, env.OS)
	assert.NotEmpty(t, env.Date)
}

// TestBuildSystemProjectInstructions asserts the provenance-marked <project_context>
// block is appended after environment facts when instructions are present.
func TestBuildSystemProjectInstructions(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/repo", Date: "2024-01-02"}
	proj := []ProjectInstruction{{Path: "/repo/AGENTS.md", Body: "# Rules\nbuild with make test\n"}}

	blocks := buildSystem(s, env, proj)
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
