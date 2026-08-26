package refs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sinkCapturer records tool starts so the expander's display order is visible.
type sinkCapturer struct {
	starts []string
}

func (s *sinkCapturer) TurnStart(agent.TurnInfo) {}
func (s *sinkCapturer) UserPrompt(string)        {}
func (s *sinkCapturer) Thinking(string)          {}
func (s *sinkCapturer) EndThinking()             {}
func (s *sinkCapturer) Text(string)              {}
func (s *sinkCapturer) EndText()                 {}
func (s *sinkCapturer) ToolStart(_ agent.ToolCall, label string) func(agent.ToolResult) {
	s.starts = append(s.starts, label)
	return func(agent.ToolResult) {}
}
func (s *sinkCapturer) ToolOutput(string, string)       {}
func (s *sinkCapturer) ToolProgress(agent.ToolProgress) {}
func (s *sinkCapturer) Diff(string, string, string)     {}
func (s *sinkCapturer) Usage(llm.Usage)                 {}
func (s *sinkCapturer) Context(tokens.ContextState)     {}
func (s *sinkCapturer) Notice(string, agent.Level)      {}
func (s *sinkCapturer) TurnEnd(agent.TurnResult)        {}

// newExpander builds a real tools registry rooted at dir and an expander.
func newExpander(t *testing.T, dir string) (*Expander, *sinkCapturer) {
	t.Helper()
	reg, err := tools.Builtins(tools.Options{Cwd: dir, SessionID: "refstest"})
	require.NoError(t, err)
	sink := &sinkCapturer{}
	return NewExpander(reg, sink, tools.PathPolicy{Cwd: dir}), sink
}

func TestExpand(t *testing.T) {
	t.Run("tilde_path_injects_read", func(t *testing.T) {
		// not parallel: Setenv mutates the process HOME.
		root := t.TempDir() // workspace cwd
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(home, "notes.md"), []byte("hi\n"), 0o600))
		t.Setenv("HOME", home)

		x, sink := newExpander(t, root)
		res := x.Expand("see @~/notes.md")
		assert.Equal(t, "see @~/notes.md", res.Text)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "read ~/notes.md")
	})

	t.Run("tilde_annotates_large_file", func(t *testing.T) {
		// not parallel: Setenv mutates the process HOME.
		root := t.TempDir()
		home := t.TempDir()
		writeBigLines(t, home, "big.md")
		t.Setenv("HOME", home)

		x, _ := newExpander(t, root)
		res := x.Expand("see @~/big.md")
		assert.Contains(t, res.Text, "@~/big.md (")
		assert.Empty(t, injected(t, res))
	})

	t.Run("no_refs", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)
		res := x.Expand("just a message")
		assert.Equal(t, "just a message", res.Text)
		assert.Empty(t, injected(t, res))
	})

	t.Run("small_file_injects_read", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("look at @a.go")
		assert.Equal(t, "look at @a.go", res.Text)
		assert.Positive(t, res.Est)
		msgs := injected(t, res)
		require.Len(t, msgs, 2)
		assert.Equal(t, llm.RoleAssistant, msgs[0].Role)
		assert.Equal(t, llm.RoleUser, msgs[1].Role)
		assert.Contains(t, sink.starts, "read a.go")
	})

	t.Run("wildcard_injects_ls_listing_matches", func(t *testing.T) {
		dir := t.TempDir()
		for _, f := range []string{"a.go", "b.txt"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("hi\n"), 0o600))
		}
		x, sink := newExpander(t, dir)

		res := x.Expand("see @*.txt")
		assert.Equal(t, "see @*.txt", res.Text) // the pattern stays literal in prose
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "ls *.txt")
	})

	t.Run("directory_injects_ls", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, sink := newExpander(t, dir)

		res := x.Expand("list @sub")
		assert.Equal(t, "list @sub", res.Text)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "ls sub")
	})

	t.Run("directory_ls_runs_when_disabled", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, _ := newExpander(t, dir)
		x.reg.SetEnabled([]string{"read", "write", "edit"}) // ls disabled

		res := x.Expand("list @sub")
		require.Len(t, injected(t, res), 2)
	})

	t.Run("large_file_annotates", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		res := x.Expand("see @big.go")
		assert.Contains(t, res.Text, "@big.go (")
		assert.Contains(t, res.Text, "lines")
		assert.Empty(t, injected(t, res))
	})

	t.Run("binary_file_annotates", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2, 3, 4, 5}, 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @blob.bin")
		assert.Contains(t, res.Text, "@blob.bin (binary,")
		assert.Empty(t, injected(t, res))
	})

	t.Run("missing_path_stays_literal", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand("see @nope.go")
		assert.Equal(t, "see @nope.go", res.Text)
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "not found")
		assert.Empty(t, injected(t, res))
	})

	t.Run("dedupes_unchanged_file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// first expand reads it (observes in the tracker)
		res1 := x.Expand("@a.go")
		require.Len(t, injected(t, res1), 2)

		// second expand: file unchanged in the tracker, nothing injected
		res2 := x.Expand("@a.go")
		assert.Empty(t, injected(t, res2))
	})

	t.Run("reinjects_when_file_changes", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// first inclusion reads the file and injects it
		res1 := x.Expand("@a.go")
		require.Len(t, injected(t, res1), 2)

		// unchanged: not injected again
		res2 := x.Expand("@a.go")
		assert.Empty(t, injected(t, res2))

		// hash changed: must be read and injected again
		require.NoError(t, os.WriteFile(p, []byte("package a\nvar X = 1\n"), 0o600))
		res3 := x.Expand("@a.go")
		require.Len(t, injected(t, res3), 2)

		// unchanged again: deduped once more
		res4 := x.Expand("@a.go")
		assert.Empty(t, injected(t, res4))
	})

	t.Run("replaces_existing_annotation", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		// re-expand the already-annotated text: the measurement is replaced, not doubled
		annotated := "see @big.go (999 lines, 9mb)"
		res := x.Expand(annotated)
		assert.Contains(t, res.Text, "@big.go (")
		assert.NotContains(t, res.Text, "(999", "old annotation replaced")
		assert.False(t, strings.Contains(res.Text, "(600 lines") && strings.Contains(res.Text, "(999"),
			"no double annotation")
	})

	t.Run("reads_land_in_transcript_order", func(t *testing.T) {
		dir := t.TempDir()
		for _, f := range []string{"a.go", "b.txt"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("hi\n"), 0o600))
		}
		x, _ := newExpander(t, dir)

		res := x.Expand("@a.go and @b.txt")
		ids := toolCallIDs(injected(t, res))
		assert.Equal(t, []string{"ref-1-a.go", "ref-1-b.txt"}, ids,
			"reads must land in forward document order, not reversed")
	})

	t.Run("mixed_dirs_and_reads_keep_order", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("hi\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("@a.go then @sub")
		assert.Equal(t, []string{"ref-1-a.go", "ref-1-ls-sub"}, toolCallIDs(injected(t, res)),
			"mixed read and ls references keep document order")
	})

	t.Run("nothing_to_inject_has_no_run", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		assert.Nil(t, x.Expand("plain message").Run)
		assert.Nil(t, x.Expand("see @nope.go").Run)
	})

	t.Run("same_path_twice_injects_once", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, _ := newExpander(t, dir)

		res := x.Expand("compare @a.go with @a.go, then @sub and @sub")
		// a repeated path must not duplicate its content, and must never repeat a
		// call id: Anthropic rejects a request whose tool_use ids collide
		assert.Equal(t, []string{"ref-1-a.go", "ref-1-ls-sub"}, toolCallIDs(injected(t, res)))
	})

	t.Run("same_path_twice_counts_once", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		once := x.Expand("see @a.go")
		twice := x.Expand("see @a.go and @a.go")
		assert.Equal(t, once.Est, twice.Est) // the running byte budget counts it once
	})

	t.Run("batch_reads_path_once", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// two messages joined into one batch: both planned before either read
		first := x.Expand("look at @a.go")
		second := x.Expand("and @a.go again")
		require.NotNil(t, first.Run)
		require.NotNil(t, second.Run)

		assert.Len(t, injected(t, first), 2)
		assert.Empty(t, injected(t, second), "the first item's read already landed")
	})

	t.Run("cancelled_context_stops_the_batch", func(t *testing.T) {
		dir := t.TempDir()
		for _, f := range []string{"a.go", "b.go"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("package a\n"), 0o600))
		}
		x, _ := newExpander(t, dir)
		res := x.Expand("@a.go and @b.go")
		require.NotNil(t, res.Run)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.Empty(t, res.Run(ctx))
	})

	t.Run("repeat_reference_mints_new_id", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		first := toolCallIDs(injected(t, x.Expand("@a.go")))
		require.NoError(t, os.WriteFile(p, []byte("package a\nvar X = 1\n"), 0o600))
		second := toolCallIDs(injected(t, x.Expand("@a.go")))

		require.Len(t, first, 1)
		require.Len(t, second, 1)
		assert.NotEqual(t, first[0], second[0], "a re-read of the same path needs a fresh call id")
	})
}

func TestExpanderSeed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
	x, _ := newExpander(t, dir)

	x.Seed([]llm.Message{{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: "ref-4-a.go", Name: "read"},
		llm.ToolCallBlock{ID: "call_9", Name: "bash"}, // an unrelated id never seeds
	}}})

	assert.Equal(t, []string{"ref-5-a.go"}, toolCallIDs(injected(t, x.Expand("@a.go"))))
}

// writeBigLines writes name under dir with enough lines to exceed RefInject.
func writeBigLines(t *testing.T, dir string, name string) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 600; i++ {
		b.WriteString("line\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600))
}

// injected runs a result's planned reads and returns the messages they produce.
func injected(t *testing.T, res Result) []llm.Message {
	t.Helper()
	if res.Run == nil {
		return nil
	}
	return res.Run(t.Context())
}

// toolCallIDs returns the assistant tool-call ids in a message slice.
func toolCallIDs(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if len(m.Content) > 0 {
			if tc, ok := m.Content[0].(llm.ToolCallBlock); ok && tc.ID != "" {
				out = append(out, tc.ID)
			}
		}
	}
	return out
}

func TestExpandReserveMatchesLandedPairs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var src strings.Builder
	for i := range 400 { // enough lines that the number prefix is a real share
		src.WriteString("\tfmt.Println(\"line ")
		src.WriteString(strconv.Itoa(i))
		src.WriteString("\")\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.go"), []byte(src.String()), 0o600))

	x, _ := newExpander(t, dir)
	res := x.Expand("look at @big.go")
	require.NotNil(t, res.Run)
	require.NotZero(t, res.Est)

	landed := tokens.EstimateMessages(res.Run(t.Context()))
	require.NotZero(t, landed)

	// the reserve holds the tokens from submit until the pair is appended, so it has
	// to size exactly what lands: line-numbered text plus the call and result framing.
	// Reserving the raw file size alone ran 16-20% short on a source file.
	assert.Equal(t, landed, res.Est)
}

func TestExpandReservesListings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "a.go"), []byte("package a\n"), 0o600))

	// a listing cannot be measured without doing the walk, but reserving nothing at
	// all left a directory or glob reference entirely absent from the bar
	for _, ref := range []string{"@sub/", "@sub/*.go"} {
		t.Run(ref, func(t *testing.T) {
			x, _ := newExpander(t, dir)
			res := x.Expand("see " + ref)
			require.NotNil(t, res.Run)
			assert.Positive(t, res.Est)
		})
	}
}
