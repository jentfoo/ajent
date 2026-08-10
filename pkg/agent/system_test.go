package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func TestBuildSystem(t *testing.T) {
	t.Parallel()

	s := &State{Model: llm.Model{ID: "test"}}
	env := Environment{Cwd: "/repo", OS: "linux/amd64", Shell: "bash", Date: "2024-01-02", Branch: "main", Dirty: true}

	blocks := buildSystem(s, env)
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

	b1, _ := buildSystem(s, env1)[0].(llm.TextBlock)
	b2, _ := buildSystem(s, env2)[0].(llm.TextBlock)
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
