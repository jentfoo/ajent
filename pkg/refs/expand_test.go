package refs

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// sinkCapturer records tool starts so the expander's display order is visible.
type sinkCapturer struct {
	starts []string
}

func (s *sinkCapturer) TurnStart(agent.TurnInfo) {}
func (s *sinkCapturer) UserPrompt(string)        {}
func (s *sinkCapturer) InjectContext(string)     {}
func (s *sinkCapturer) Thinking(string)          {}
func (s *sinkCapturer) EndThinking()             {}
func (s *sinkCapturer) Text(string)              {}
func (s *sinkCapturer) EndText()                 {}
func (s *sinkCapturer) ToolStart(_ agent.ToolCall, label string, _ bool) func(agent.ToolResult) {
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

	return newVisionExpander(t, dir, nil)
}

// newVisionExpander is newExpander with an explicit vision gate, shared by the
// read tool and the expander so both arms agree.
func newVisionExpander(t *testing.T, dir string, vision func() bool) (*Expander, *sinkCapturer) {
	t.Helper()

	reg, err := tools.Builtins(tools.Options{Cwd: dir, SessionID: "refstest", Vision: vision})
	require.NoError(t, err)
	sink := &sinkCapturer{}
	return NewExpander(reg, sink, tools.PathPolicy{Cwd: dir}, vision), sink
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

	// an existing bracketed dir lists literally and dedupes on repeat
	t.Run("metachar_dir_lists_literal", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "weird[1]"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "weird1.txt"), []byte("x\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("list @weird[1]")
		assert.Equal(t, "list @weird[1]", res.Text)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "ls weird[1]")

		// observed once: a repeat is deduped like any dir listing
		assert.Empty(t, injected(t, x.Expand("list @weird[1] again")))
	})

	// an existing bracketed file reads literally, never glob-matches the sibling
	t.Run("metachar_file_reads_literal", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "weird[1].txt"), []byte("content\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "weird1.txt"), []byte("sibling\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("see @weird[1].txt")
		assert.Empty(t, res.Notices)
		require.Len(t, injected(t, res), 2) // a read pair, not an ls error result
		assert.Contains(t, sink.starts, "read weird[1].txt")
	})

	// a metacharacter name matching nothing real still plans the pattern's ls
	t.Run("metachar_pattern_still_lists", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "weird1.txt"), []byte("x\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("see @weird[2]")
		assert.Empty(t, res.Notices) // the pattern warns through its ls result, not a notice
		assert.Equal(t, []string{"ref-1-ls-weird[2]"}, toolCallIDs(injected(t, res)))
		assert.Contains(t, sink.starts, "ls weird[2]")
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

	// the escaped form completion inserts parses back to the real name and reads it
	t.Run("escaped_ref_injects_read", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "my notes.txt"), []byte("hello\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("see @my\\ notes.txt")
		assert.Equal(t, "see @my\\ notes.txt", res.Text)
		assert.Empty(t, res.Notices)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "read my notes.txt")
	})

	// re-expanding the annotated text replaces the measurement, never stacks one
	t.Run("escaped_annotation_round_trips", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big notes.go")
		x, _ := newExpander(t, dir)

		res := x.Expand("see @big\\ notes.go")
		assert.Contains(t, res.Text, "@big\\ notes.go (")
		assert.Empty(t, injected(t, res))

		again := x.Expand(res.Text)
		assert.Contains(t, again.Text, "@big\\ notes.go (")
		assert.Equal(t, 1, strings.Count(again.Text, "lines"))
	})

	// a missing escaped name warns with the natural spelling, not the escapes
	t.Run("escaped_missing_warns_decoded", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand("see @my\\ notes.txt")
		assert.Equal(t, "see @my\\ notes.txt", res.Text)
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@my notes.txt not found")
		assert.Empty(t, injected(t, res))
	})

	// sentence-final '.' and ':' ride inside a resolving token
	t.Run("sentence_punct_trims_to_file", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			file string // what the trimmed token must read
		}{
			{"period", "Explain @main.go.", "main.go"},
			{"colon", "see @a.txt: it explains", "a.txt"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, c.file), []byte("hi\n"), 0o600))
				x, sink := newExpander(t, dir)

				res := x.Expand(c.in)
				assert.Equal(t, c.in, res.Text) // prose keeps the punctuation
				assert.Empty(t, res.Notices)
				require.Len(t, injected(t, res), 2)
				assert.Contains(t, sink.starts, "read "+c.file)
			})
		}
	})

	// piped headless input keeps interior CRLF; every line-ending ref must still resolve
	t.Run("crlf_line_endings_resolve", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hi\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("see @a.go\r\nand @b.txt")
		assert.Empty(t, res.Notices) // no "@a.go\r not found"
		require.Len(t, injected(t, res), 4)
		assert.Contains(t, sink.starts, "read a.go")
		assert.Contains(t, sink.starts, "read b.txt")
	})

	t.Run("trailing_period_dir_lists", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, sink := newExpander(t, dir)

		res := x.Expand("list @sub.")
		assert.Empty(t, res.Notices)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "ls sub")
	})

	t.Run("dot_slash_ref_trims", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, sink := newExpander(t, dir)

		res := x.Expand("see @./a.go.")
		assert.Empty(t, res.Notices)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "read ./a.go")
	})

	// an existing written form wins, keeping its trailing dot
	t.Run("existing_written_form_wins", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"cfg", "cfg."} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600))
		}
		x, sink := newExpander(t, dir)

		res := x.Expand("see @cfg.")
		assert.Empty(t, res.Notices)
		require.Len(t, injected(t, res), 2)
		assert.Contains(t, sink.starts, "read cfg.")
	})

	t.Run("missing_with_period_warns", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand("see @nope.")
		assert.Equal(t, "see @nope.", res.Text)
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@nope. not found")
	})

	// an all-punctuation token trims to nothing: the written form is what warns
	t.Run("all_punct_token_warns", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand("see @...")
		assert.Equal(t, "see @...", res.Text)
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@... not found")
	})

	// the trimmed ref annotates under its written token and survives re-expansion
	t.Run("trimmed_large_file_annotates", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		res := x.Expand("see @big.go.")
		assert.Contains(t, res.Text, "@big.go. (")
		assert.Empty(t, injected(t, res))

		again := x.Expand(res.Text)
		assert.Equal(t, 1, strings.Count(again.Text, "lines"))
	})

	t.Run("changed_file_reinjects_new_id", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		require.NoError(t, os.WriteFile(p, []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		// first inclusion reads the file; an unchanged re-expand dedupes
		first := toolCallIDs(injected(t, x.Expand("@a.go")))
		require.Len(t, first, 1)
		assert.Empty(t, injected(t, x.Expand("@a.go")))

		// hash changed: read and injected again under a fresh call id
		require.NoError(t, os.WriteFile(p, []byte("package a\nvar X = 1\n"), 0o600))
		again := toolCallIDs(injected(t, x.Expand("@a.go")))
		require.Len(t, again, 1)
		assert.NotEqual(t, first[0], again[0])

		// unchanged again: deduped once more
		assert.Empty(t, injected(t, x.Expand("@a.go")))
	})

	t.Run("replaces_existing_annotation", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		// re-expand the already-annotated text: the measurement is replaced, not doubled
		annotated := "see @big.go (999 lines, 9mb)"
		res := x.Expand(annotated)
		assert.Contains(t, res.Text, "@big.go (")
		assert.NotContains(t, res.Text, "(999")
		assert.False(t, strings.Contains(res.Text, "(600 lines") && strings.Contains(res.Text, "(999"))
	})

	t.Run("shrunk_file_strips_annotation", func(t *testing.T) {
		dir := t.TempDir()
		// a file that was large and annotated, now small enough to inject: the stale
		// size claim must not survive alongside the fresh read
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @a.go (800 lines, 64kb)")
		assert.Equal(t, "see @a.go", res.Text)
		require.Len(t, injected(t, res), 2) // a fresh read is planned for the shrunk file
	})

	t.Run("repeat_ref_restrips_annotation", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @a.go (800 lines) and @a.go")
		assert.Equal(t, "see @a.go and @a.go", res.Text)
		require.Len(t, injected(t, res), 2) // one injection for both mentions
	})

	t.Run("reads_land_in_transcript_order", func(t *testing.T) {
		dir := t.TempDir()
		for _, f := range []string{"a.go", "b.txt"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("hi\n"), 0o600))
		}
		x, _ := newExpander(t, dir)

		res := x.Expand("@a.go and @b.txt")
		ids := toolCallIDs(injected(t, res))
		assert.Equal(t, []string{"ref-1-r-a.go", "ref-1-r-b.txt"}, ids)
	})

	t.Run("mixed_dirs_reads_keep_order", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("hi\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("@a.go then @sub")
		assert.Equal(t, []string{"ref-1-r-a.go", "ref-1-ls-sub"}, toolCallIDs(injected(t, res)))
	})

	// a file named "ls-"+dir must not share its tool_use id with that dir's listing
	t.Run("ls_named_file_keeps_ids_distinct", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ls-src"), []byte("x\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("diff @src and @ls-src")
		assert.Equal(t, []string{"ref-1-ls-src", "ref-1-r-ls-src"}, toolCallIDs(injected(t, res)))
	})

	// the invariant that keeps a written path from imitating another kind's id:
	// any kind prefixed by another admits a colliding path
	t.Run("no_kind_prefixes_another", func(t *testing.T) {
		for _, a := range []string{readKind, lsKind} {
			for _, b := range []string{readKind, lsKind} {
				if a == b {
					continue
				}
				assert.False(t, strings.HasPrefix(a, b))
			}
		}
	})

	t.Run("nothing_to_inject_no_run", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		assert.Nil(t, x.Expand("plain message").Run)
		assert.Nil(t, x.Expand("see @nope.go").Run)
	})

	// dedupe keeps first-mention position, never reorders
	t.Run("duplicate_keeps_first_position", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("hi\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @a.go then @sub then @a.go")
		assert.Equal(t, []string{"ref-1-r-a.go", "ref-1-ls-sub"}, toolCallIDs(injected(t, res)))
	})

	// every spelling of one file dedupes to a single planned read
	t.Run("spelling_variants_dedupe", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @a.go and @./a.go or @" + filepath.Join(dir, "a.go"))
		assert.Equal(t, []string{"ref-1-r-a.go"}, toolCallIDs(injected(t, res)))
	})

	// a repeated missing path warns once, not once per mention
	t.Run("repeat_missing_warns_once", func(t *testing.T) {
		dir := t.TempDir()
		x, _ := newExpander(t, dir)

		res := x.Expand("see @nope.go and @nope.go")
		assert.Equal(t, "see @nope.go and @nope.go", res.Text)
		require.Len(t, res.Notices, 1)
	})

	// a repeated non-text file shows its measurement at the first mention only
	t.Run("repeat_binary_annotates_once", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0, 1, 2}, 0o600))
		x, _ := newExpander(t, dir)

		res := x.Expand("see @blob.bin and @blob.bin")
		assert.Equal(t, 1, strings.Count(res.Text, "(binary,"))
	})

	// an oversized file's refusal is deduped like any other outcome
	t.Run("repeat_oversized_notices_once", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		res := x.Expand("see @big.go and @big.go again")
		assert.Contains(t, res.Text, "@big.go (")
		assert.Contains(t, res.Text, "and @big.go again") // the repeat stays bare
		require.Len(t, res.Notices, 1)
	})

	t.Run("same_path_twice_injects_once", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, _ := newExpander(t, dir)

		res := x.Expand("compare @a.go with @a.go, then @sub and @sub")
		// one injection per path; colliding call ids are rejected by the API
		assert.Equal(t, []string{"ref-1-r-a.go", "ref-1-ls-sub"}, toolCallIDs(injected(t, res)))
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
		assert.Empty(t, injected(t, second))
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

	// a listed dir dedupes while unchanged and re-lists once its contents change
	t.Run("dir_dedupes_until_changed", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o700))
		x, _ := newExpander(t, dir)

		// first @ lists it and observes the entries in the tracker
		first := x.Expand("list @sub")
		require.Len(t, injected(t, first), 2)

		// unchanged contents: not re-listed
		assert.Empty(t, injected(t, x.Expand("list @sub again")))

		// an added entry changes the listing, listed again under a fresh id
		require.NoError(t, os.WriteFile(filepath.Join(sub, "a.txt"), []byte("x"), 0o600))
		relisted := toolCallIDs(injected(t, x.Expand("list @sub")))
		assert.Equal(t, []string{"ref-3-ls-sub"}, relisted)
	})

	// an agent ls run earlier dedupes a later @ reference to the same dir
	t.Run("agent_ls_dedupes_following_ref", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		x, _ := newExpander(t, dir)

		tool, ok := x.reg.Lookup("ls")
		require.True(t, ok)
		agent.InjectPair(t.Context(), tool, agent.NopSink{},
			agent.ToolCall{ID: "agent-ls", Name: "ls", Input: []byte(`{"path":"sub"}`)}, "ls sub")

		// the agent listing observed the dir, a following @ deduped
		res := x.Expand("list @sub")
		assert.Empty(t, injected(t, res))
	})

	// a partial (windowed) read does not satisfy @; only a full one dedupes
	t.Run("partial_read_reinjects", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.txt")
		require.NoError(t, os.WriteFile(p, []byte(strings.Repeat("line\n", 40)), 0o600))
		x, _ := newExpander(t, dir)

		// the model read one line of forty through a limit
		tool, ok := x.reg.Lookup("read")
		require.True(t, ok)
		agent.InjectPair(t.Context(), tool, agent.NopSink{},
			agent.ToolCall{ID: "agent-read", Name: "read", Input: []byte(`{"path":"a.txt","limit":1}`)}, "read a.txt")

		res := x.Expand("see @a.txt")
		require.Len(t, injected(t, res), 2) // the window is not the file

		// that injection ran a full read, so a following ref dedupes
		assert.Empty(t, injected(t, x.Expand("see @a.txt again")))
	})
}

func TestExpanderSeed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
	x, _ := newExpander(t, dir)

	x.Seed([]llm.Message{{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: "ref-4-r-a.go", Name: "read"},
		llm.ToolCallBlock{ID: "call_9", Name: "bash"}, // an unrelated id never seeds
	}}})

	assert.Equal(t, []string{"ref-5-r-a.go"}, toolCallIDs(injected(t, x.Expand("@a.go"))))
}

func TestExpandTotalCap(t *testing.T) {
	t.Parallel()

	// sized near half of RefTotal, under RefInject: only the total cap trips
	t.Run("repeat_mention_not_annotated", func(t *testing.T) {
		dir := t.TempDir()
		writeSizedLines(t, dir, "half.txt", 100, 900)
		x, _ := newExpander(t, dir)

		res := x.Expand("see @half.txt and @half.txt again")
		assert.Equal(t, "see @half.txt and @half.txt again", res.Text)
		assert.Empty(t, res.Notices)
		assert.Equal(t, []string{"ref-1-r-half.txt"}, toolCallIDs(injected(t, res)))
	})

	// an unchanged file reserves nothing against the total cap
	t.Run("unchanged_file_not_annotated", func(t *testing.T) {
		dir := t.TempDir()
		writeSizedLines(t, dir, "a.txt", 100, 900)
		writeSizedLines(t, dir, "b.txt", 100, 900)
		x, _ := newExpander(t, dir)

		// the first mention lands the read, so the tracker holds a.txt unchanged
		require.Len(t, injected(t, x.Expand("read @a.txt")), 2)

		res := x.Expand("compare @a.txt with @b.txt")
		assert.Equal(t, "compare @a.txt with @b.txt", res.Text)
		assert.Empty(t, res.Notices)
		assert.Equal(t, []string{"ref-2-r-b.txt"}, toolCallIDs(injected(t, res)))
	})

	// fresh files fill the cap in mention order; overflow declines by budget
	t.Run("distinct_files_annotate_overflow", func(t *testing.T) {
		dir := t.TempDir()
		writeSizedLines(t, dir, "a.txt", 100, 900)
		writeSizedLines(t, dir, "b.txt", 100, 900)
		x, _ := newExpander(t, dir)

		res := x.Expand("compare @a.txt with @b.txt")
		assert.NotContains(t, res.Text, "@a.txt (")
		assert.Contains(t, res.Text, "@b.txt (")
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@b.txt exceeds the remaining reference budget; annotated")
		assert.Equal(t, []string{"ref-1-r-a.txt"}, toolCallIDs(injected(t, res)))
	})

	// a file over RefInject on its own is refused for its shape, wording distinct
	// from the total-budget decline so neither reason masquerades as the other
	t.Run("per_file_refusal_names_size", func(t *testing.T) {
		dir := t.TempDir()
		writeBigLines(t, dir, "big.go")
		x, _ := newExpander(t, dir)

		res := x.Expand("see @big.go")
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@big.go too large; annotated")
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

// writeSizedLines writes name under dir as lines text lines of lineLen bytes
// each: a file shaped to sit between half and all of the default RefTotal cap
// while staying under RefInject.
func writeSizedLines(t *testing.T, dir, name string, lines, lineLen int) {
	t.Helper()

	var b strings.Builder
	for range lines {
		b.WriteString(strings.Repeat("x", lineLen))
		b.WriteByte('\n')
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

func TestExpandReserve(t *testing.T) {
	t.Parallel()

	// the reserved estimate for a large source-file reference exactly matches what lands
	t.Run("matches_landed_pairs", func(t *testing.T) {
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

		// the reserve must match what lands: framed, line-numbered content
		assert.Equal(t, landed, res.Est)
	})

	// unmeasurable listings still reserve above zero
	t.Run("lists_listings", func(t *testing.T) {
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
	})
}

// writePNG writes a small valid png as name under dir.
func writePNG(t *testing.T, dir string, name string) {
	t.Helper()

	m := image.NewRGBA(image.Rect(0, 0, 6, 4))
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o600))
}

func TestExpandImages(t *testing.T) {
	t.Run("vision_injects_image_read", func(t *testing.T) {
		dir := t.TempDir()
		writePNG(t, dir, "pic.png")
		x, sink := newVisionExpander(t, dir, func() bool { return true })

		res := x.Expand("look at @pic.png")
		assert.Equal(t, "look at @pic.png", res.Text)
		require.NotNil(t, res.Run)
		msgs := injected(t, res)
		require.Len(t, msgs, 2)
		resBlk, ok := msgs[1].Content[0].(llm.ToolResultBlock)
		require.True(t, ok)
		assert.False(t, resBlk.IsError)
		require.Len(t, resBlk.Content, 1)
		img, ok := resBlk.Content[0].(llm.ImageBlock)
		require.True(t, ok)
		assert.Equal(t, "image/png", img.MediaType)
		assert.Contains(t, sink.starts, "read pic.png")
	})

	t.Run("text_only_annotates", func(t *testing.T) {
		dir := t.TempDir()
		writePNG(t, dir, "pic.png")
		x, _ := newVisionExpander(t, dir, nil) // nil vision: today's annotation

		res := x.Expand("look at @pic.png")
		assert.Contains(t, res.Text, "@pic.png (image,")
		assert.Empty(t, injected(t, res))
	})

	t.Run("repeat_ref_plans_once", func(t *testing.T) {
		dir := t.TempDir()
		writePNG(t, dir, "pic.png")
		x, _ := newVisionExpander(t, dir, func() bool { return true })

		res := x.Expand("look at @pic.png and @pic.png again")
		assert.Equal(t, "look at @pic.png and @pic.png again", res.Text)
		msgs := injected(t, res)
		require.Len(t, msgs, 2) // one call + result pair
	})

	// a text-only model annotates once per path however often it is named
	t.Run("repeat_ref_annotates_once", func(t *testing.T) {
		dir := t.TempDir()
		writePNG(t, dir, "pic.png")
		x, _ := newVisionExpander(t, dir, nil)

		res := x.Expand("look at @pic.png and @pic.png again")
		assert.Equal(t, 1, strings.Count(res.Text, "(image,"))
	})

	// image mentions pay the shared total cap like text reads
	t.Run("image_mentions_pay_total_cap", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"one.png", "two.png", "three.png"} {
			writePNG(t, dir, name)
		}
		prev := tools.RefTotalLimit()
		tools.ApplyLimits(tools.Limits{RefTotal: tools.Limit{Bytes: 2 * imageNominalBytes}})
		t.Cleanup(func() { tools.ApplyLimits(tools.Limits{RefTotal: prev}) })

		x, _ := newVisionExpander(t, dir, func() bool { return true })
		res := x.Expand("look at @one.png @two.png and @three.png")

		assert.Equal(t, []string{"ref-1-r-one.png", "ref-1-r-two.png"},
			toolCallIDs(injected(t, res))) // the first two fit under the cap
		require.Len(t, res.Notices, 1)
		assert.Contains(t, res.Notices[0], "@three.png exceeds the remaining reference budget; annotated")
		assert.Contains(t, res.Text, "@three.png (image,") // declined stays visible with its shape
	})
}
