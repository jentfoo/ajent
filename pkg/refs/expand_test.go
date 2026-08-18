package refs

import (
	"os"
	"path/filepath"
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
		res := x.Expand(t.Context(), "see @~/notes.md")
		assert.Equal(t, "see @~/notes.md", res.Text)
		require.Len(t, res.Before, 2, "a home-dir ref injects a read pair")
		assert.Contains(t, sink.starts, "read ~/notes.md")
	})

	t.Run("tilde_annotates_large_file", func(t *testing.T) {
		// not parallel: Setenv mutates the process HOME.
		root := t.TempDir()
		home := t.TempDir()
		writeBigLines(t, home, "big.md")
		t.Setenv("HOME", home)

		x, _ := newExpander(t, root)
		res := x.Expand(t.Context(), "see @~/big.md")
		assert.Contains(t, res.Text, "@~/big.md (", "large home-dir file annotated in place")
		assert.Empty(t, res.Before)
	})

	t.Run("no_refs", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)
		res := x.Expand(t.Context(), "just a message")
		assert.Equal(t, "just a message", res.Text)
		assert.Empty(t, res.Before)
	})

	t.Run("small_file_injects_read", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand(t.Context(), "look at @a.go")
		assert.Equal(t, "look at @a.go", res.Text)
		require.Len(t, res.Before, 2, "one call + result pair")
		assert.Equal(t, llm.RoleAssistant, res.Before[0].Role)
		assert.Equal(t, llm.RoleUser, res.Before[1].Role)
		assert.Contains(t, sink.starts, "read a.go")
	})

	t.Run("directory_injects_ls", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, sink := newExpander(t, dir)

		res := x.Expand(t.Context(), "list @sub")
		assert.Equal(t, "list @sub", res.Text)
		require.Len(t, res.Before, 2)
		assert.Contains(t, sink.starts, "ls sub")
	})

	t.Run("directory_ls_runs_when_disabled", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, _ := newExpander(t, dir)
		x.reg.SetEnabled([]string{"read", "write", "edit"}) // ls disabled

		res := x.Expand(t.Context(), "list @sub")
		require.Len(t, res.Before, 2, "ls runs regardless of enabled state")
	})

	t.Run("large_file_annotates", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		res := x.Expand(t.Context(), "see @big.go")
		assert.Contains(t, res.Text, "@big.go (", "large file annotated in place")
		assert.Contains(t, res.Text, "lines")
		assert.Empty(t, res.Before, "nothing injected for a large file")
	})

	t.Run("binary_file_annotates", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2, 3, 4, 5}, 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand(t.Context(), "see @blob.bin")
		assert.Contains(t, res.Text, "@blob.bin (binary,")
		assert.Empty(t, res.Before)
	})

	t.Run("missing_path_stays_literal", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand(t.Context(), "see @nope.go")
		assert.Equal(t, "see @nope.go", res.Text)
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "not found")
		assert.Empty(t, res.Before)
	})

	t.Run("dedupes_unchanged_file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// first expand reads it (observes in the tracker)
		res1 := x.Expand(t.Context(), "@a.go")
		require.Len(t, res1.Before, 2)

		// second expand: file unchanged in the tracker → nothing injected
		res2 := x.Expand(t.Context(), "@a.go")
		assert.Empty(t, res2.Before, "an unchanged, already-read file is not re-injected")
	})

	t.Run("reinjects_when_file_changes", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// first inclusion reads the file and injects it
		res1 := x.Expand(t.Context(), "@a.go")
		require.Len(t, res1.Before, 2, "first @ include must inject the read pair")

		// unchanged → not injected again
		res2 := x.Expand(t.Context(), "@a.go")
		assert.Empty(t, res2.Before, "unchanged, already-read file is not re-injected")

		// hash changed → must be read and injected again
		require.NoError(t, os.WriteFile(p, []byte("package a\nvar X = 1\n"), 0o600))
		res3 := x.Expand(t.Context(), "@a.go")
		require.Len(t, res3.Before, 2, "an @ is re-injected after its hash changes")

		// unchanged again → deduped once more
		res4 := x.Expand(t.Context(), "@a.go")
		assert.Empty(t, res4.Before, "dedups again once the changed content has been read")
	})

	t.Run("replaces_existing_annotation", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		// re-expand the already-annotated text: the measurement is replaced, not doubled
		annotated := "see @big.go (999 lines, 9mb)"
		res := x.Expand(t.Context(), annotated)
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

		res := x.Expand(t.Context(), "@a.go and @b.txt")
		ids := toolCallIDs(res.Before)
		assert.Equal(t, []string{"ref-a.go", "ref-b.txt"}, ids,
			"reads must land in forward document order, not reversed")
	})

	t.Run("mixed_dirs_and_reads_keep_order", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("hi\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand(t.Context(), "@a.go then @sub")
		assert.Equal(t, []string{"ref-a.go", "ref-ls-sub"}, toolCallIDs(res.Before),
			"mixed read and ls references keep document order")
	})
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
