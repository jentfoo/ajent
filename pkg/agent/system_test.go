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

	// the whole system prompt is a single text block carrying env facts.
	t.Run("base", func(t *testing.T) {
		s := &State{Model: llm.Model{ID: "test"}}
		env := Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"}

		blocks := buildSystem(s, env, nil, nil)
		assert.Len(t, blocks, 1)

		tb, ok := blocks[0].(llm.TextBlock)
		require.True(t, ok) // a single text block is the whole system prompt
		for _, want := range []string{"/repo", "linux/amd64", "2024-01-02"} {
			assert.Contains(t, tb.Text, want)
		}
	})

	// equal inputs produce byte-identical blocks so the provider prompt cache survives between requests.
	t.Run("cache_stable", func(t *testing.T) {
		s := &State{Model: llm.Model{ID: "test"}}
		env := Environment{Cwd: "/r", OS: "linux", Date: "2024-01-02 09:00"}

		b1, ok := buildSystem(s, env, nil, nil)[0].(llm.TextBlock)
		require.True(t, ok)
		b2, ok := buildSystem(s, env, nil, nil)[0].(llm.TextBlock)
		require.True(t, ok)
		assert.Equal(t, b1.Text, b2.Text)
	})

	// the date changes at day granularity, not sub-day.
	t.Run("date_day_granular", func(t *testing.T) {
		s := &State{Model: llm.Model{ID: "test"}}
		b1, ok := buildSystem(s, Environment{Cwd: "/r", OS: "linux", Date: "2024-01-02 09:00"}, nil, nil)[0].(llm.TextBlock)
		require.True(t, ok)
		b2, ok := buildSystem(s, Environment{Cwd: "/r", OS: "linux", Date: "2024-01-03 08:59"}, nil, nil)[0].(llm.TextBlock)
		require.True(t, ok)

		assert.NotEqual(t, b1.Text, b2.Text) // the date differs across days
	})

	// the ls-style directory listing is emitted.
	t.Run("lists_cwd", func(t *testing.T) {
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
	})

	// an absent cwd is tolerated.
	t.Run("omits_missing_cwd_listing", func(t *testing.T) {
		s := &State{Model: llm.Model{ID: "test"}}
		blocks := buildSystem(s, Environment{Cwd: "/does/not/exist"}, nil, nil)
		tb, ok := blocks[0].(llm.TextBlock)
		require.True(t, ok)

		assert.Contains(t, tb.Text, "Working directory: /does/not/exist\n")
		assert.NotContains(t, tb.Text, "Directory contents:")
	})

	// snippets land after project instructions, each separated by a blank line.
	t.Run("snippets_append_after_project", func(t *testing.T) {
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
	})

	// the provenance-marked <project_context> block is appended after environment facts.
	t.Run("project_instructions", func(t *testing.T) {
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
	})
}

// TestIdentityLine pins the domain-neutral opening sentence so a wording change is
// deliberate and reviewed.
func TestIdentityLine(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "You help by following the user's instructions: research and review until you understand them, then focus on what is asked.\n\n", identityLine())
}

// TestDetectEnvironment exercises the real machine-fact probes.
func TestDetectEnvironment(t *testing.T) {
	t.Parallel()

	env := DetectEnvironment()
	assert.NotEmpty(t, env.Cwd)
	assert.NotEmpty(t, env.OS)
	assert.NotEmpty(t, env.Date)
}

func TestListCwd(t *testing.T) {
	t.Parallel()

	// names are sorted and directories marked.
	t.Run("sorts_and_marks_dirs", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("// x\n"), 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "a"), 0o755))

		assert.Equal(t, []string{"a/", "b.go"}, listCwd(dir))
	})

	// an absent dir returns nil.
	t.Run("missing_returns_nil", func(t *testing.T) {
		assert.Nil(t, listCwd("/does/not/exist"))
	})
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

func TestLoadProjectInstructions(t *testing.T) {
	t.Parallel()

	// reads AGENTS.md from a dir when present.
	t.Run("reads_when_present", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Rules\n"), 0o644))

		proj, err := LoadProjectInstructions(dir)
		require.NoError(t, err)
		assert.Len(t, proj, 1)
		assert.Equal(t, filepath.Join(dir, "AGENTS.md"), proj[0].Path)
		assert.Equal(t, "# Rules\n", proj[0].Body)
	})

	// returns nil when no AGENTS.md exists.
	t.Run("missing_returns_nil", func(t *testing.T) {
		proj, err := LoadProjectInstructions(t.TempDir())
		require.NoError(t, err)
		assert.Nil(t, proj)
	})

	// orders global before project.
	t.Run("layers_global_before_project", func(t *testing.T) {
		global := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("# Global\n"), 0o644))
		project := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("# Project\n"), 0o644))

		proj, err := LoadProjectInstructions(global, project)
		require.NoError(t, err)
		require.Len(t, proj, 2)
		assert.Equal(t, filepath.Join(global, "AGENTS.md"), proj[0].Path)
		assert.Equal(t, filepath.Join(project, "AGENTS.md"), proj[1].Path)
	})

	// tolerates a missing home and an absent cwd.
	t.Run("skips_empty_and_absent", func(t *testing.T) {
		proj, err := LoadProjectInstructions("", t.TempDir(), "")
		require.NoError(t, err)
		assert.Nil(t, proj)
	})
}
