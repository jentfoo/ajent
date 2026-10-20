package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestRead(t *testing.T) {
	t.Parallel()

	// a happy path reads line-numbered content with a display header
	t.Run("happy_path", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "line one\nline two\n")
		res := e.readExec(t.Context(), `{"path":"a.txt"}`)
		assert.False(t, res.IsError)
		require.Len(t, res.Content, 1)
		out := textOf(res)
		assert.Contains(t, out, "     1\tline one") // line-numbered output
		assert.Contains(t, out, "     2\tline two")
		// Display adds a section header naming the file and range, while content stays bare.
		assert.Equal(t, res.Display, "a.txt:1-2\n"+textOf(res))
	})

	t.Run("display_counts_honor_offset_and_limit", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		var b strings.Builder
		for i := 1; i <= 10; i++ {
			_, _ = fmt.Fprintf(&b, "line %d\n", i)
		}
		e.writeFile("big.txt", b.String()) // each line: "line N" = 6 runes + newline

		res := e.readExec(t.Context(), `{"path":"big.txt","offset":2,"limit":3}`) // lines 2-4
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "     2\tline 2")
		assert.NotContains(t, out, "line 5") // limit respected
		// Display mirrors the line-numbered block (no truncation marker), with paging honored
		// via a header naming the exact range read.
		assert.Contains(t, res.Display, "big.txt:2-4\n")
		assert.Contains(t, res.Display, "     2\tline 2")
		assert.NotContains(t, res.Display, "... truncated at line 5")
	})

	t.Run("missing_file_is_error_result", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.readExec(t.Context(), `{"path":"nope.txt"}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), "no such file")
	})

	t.Run("malformed_args_is_error_result", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.readExec(t.Context(), `not json`)
		assert.True(t, res.IsError) // a bad accumulator must degrade, not panic
	})

	t.Run("binary_refused", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "bin.dat"), []byte{1, 0, 2, 3}, 0o644))
		res := e.readExec(t.Context(), `{"path":"bin.dat"}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), "binary")
	})

	t.Run("unbounded_read_marks_next_offset", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		var b strings.Builder
		for i := 1; i <= 3000; i++ {
			_, _ = fmt.Fprintf(&b, "line %d\n", i)
		}
		e.writeFile("big.txt", b.String())

		res := e.readExec(t.Context(), `{"path":"big.txt"}`) // no limit: tool cuts at its bound
		assert.False(t, res.IsError)
		out := textOf(res)
		// the Display header names exactly what was shown, even when cut short
		assert.Contains(t, res.Display, fmt.Sprintf("big.txt:1-%d\n", ReadFileLimit().Lines))
		assert.Contains(t, out, fmt.Sprintf("... truncated at line %d of 3000 (%d more); read again with offset=%d",
			ReadFileLimit().Lines, 3000-ReadFileLimit().Lines, ReadFileLimit().Lines+1))
	})

	// an explicitly scoped window that is fully delivered is not truncation: the
	// tool handed over exactly what was asked, so no offset nudge is needed
	t.Run("scoped_read_has_no_marker", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		var b strings.Builder
		for i := 1; i <= 3000; i++ {
			_, _ = fmt.Fprintf(&b, "line %d\n", i)
		}
		e.writeFile("big.txt", b.String())

		res := e.readExec(t.Context(), `{"path":"big.txt","limit":200}`)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "line 1")
		assert.NotContains(t, out, "truncated at line") // full window, no paging hint
	})

	// a range wider than the limit is refused rather than silently paged
	t.Run("range_over_limit_is_error", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		var b strings.Builder
		for i := 1; i <= 3000; i++ {
			_, _ = fmt.Fprintf(&b, "line %d\n", i)
		}
		e.writeFile("big.txt", b.String())

		res := e.readExec(t.Context(), `{"path":"big.txt","limit":1500}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), "exceeds the maximum of 1000 lines")
		assert.Contains(t, textOf(res), "page with offset")
	})

	// an explicitly scoped window that reaches EOF is complete, so the limit itself
	// is not truncation and carries no marker
	t.Run("range_at_limit_ok", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		var b strings.Builder
		for i := 1; i <= 1200; i++ {
			_, _ = fmt.Fprintf(&b, "line %d\n", i)
		}
		e.writeFile("big.txt", b.String())

		res := e.readExec(t.Context(), `{"path":"big.txt","limit":1000}`)
		assert.False(t, res.IsError)
		assert.NotContains(t, textOf(res), "truncated at line")
	})

	t.Run("observes_tracker_for_ref_dedupe", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello\nworld\n")
		assert.False(t, e.readExec(t.Context(), `{"path":"a.txt"}`).IsError)
		path := filepath.Join(e.cwd, "a.txt")
		_, ok := e.tracker.Records()[path]
		assert.True(t, ok) // read records the file so @ref expansion can dedupe
		assert.True(t, e.tracker.Unchanged(path))
	})

	// an empty read shows everything there is, so it dedupes like any whole file
	t.Run("empty_file_observes_complete", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("empty.txt", "")
		path := filepath.Join(e.cwd, "empty.txt")

		res := e.readExec(t.Context(), `{"path":"empty.txt"}`)
		assert.False(t, res.IsError)
		assert.True(t, e.tracker.Unchanged(path))
	})

	// a windowed read shows only part of the file: @refs must re-inject rather
	// than dedupe against it
	t.Run("windowed_read_never_dedupes", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "one\ntwo\nthree\nfour\n")
		path := filepath.Join(e.cwd, "a.txt")

		res := e.readExec(t.Context(), `{"path":"a.txt","limit":2}`)
		assert.False(t, res.IsError)
		assert.Contains(t, textOf(res), "     1\tone")
		assert.NotContains(t, textOf(res), "three") // the window held
		assert.False(t, e.tracker.Unchanged(path))

		// paging to EOF does not make it whole either: lines before the offset
		// never arrived
		res = e.readExec(t.Context(), `{"path":"a.txt","offset":3,"limit":2}`)
		assert.False(t, res.IsError)
		assert.False(t, e.tracker.Unchanged(path))

		// the plain full read is what makes the file dedupeable again
		res = e.readExec(t.Context(), `{"path":"a.txt"}`)
		assert.False(t, res.IsError)
		assert.True(t, e.tracker.Unchanged(path))
	})

	// a rejected image lands nothing in context, so it must not let @refs dedupe
	t.Run("failed_image_fit_records_nothing", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		// the bare jpeg marker makes probeFile classify as image; the body is undecodable
		require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "pic.jpg"), []byte{0xff, 0xd8, 'j', 'u', 'n', 'k'}, 0o644))
		c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"pic.jpg"}`)}
		tool := &readTool{policy: e.policy, tracker: e.tracker, vision: func() bool { return true }}
		res, _ := tool.Execute(t.Context(), c, agent.NewOutput(agent.NopSink{}, "c"))

		assert.True(t, res.IsError)
		assert.Empty(t, e.tracker.Records())
	})

	// a fitted image is unpaged, so its observation is whole and dedupes like text
	t.Run("image_read_observes_complete", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "pic.png"), imagePNG(t), 0o644))
		c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"pic.png"}`)}
		tool := &readTool{policy: e.policy, tracker: e.tracker, vision: func() bool { return true }}
		res, _ := tool.Execute(t.Context(), c, agent.NewOutput(agent.NopSink{}, "c"))

		require.False(t, res.IsError)
		assert.True(t, e.tracker.Unchanged(filepath.Join(e.cwd, "pic.png")))
	})

	// a CRLF file reads as LF: the model never sees a \r
	t.Run("crlf_reads_as_lf", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "crlf.txt"), []byte("alpha\r\nbeta\r\n"), 0o644))

		res := e.readExec(t.Context(), `{"path":"crlf.txt"}`)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.NotContains(t, out, "\r")
		assert.Contains(t, out, "     1\talpha") // LF-only model-visible output
	})

	// a lone mid-line \r is preserved
	t.Run("lone_carriage_return_survives", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "cr.txt"), []byte("a\rb\n"), 0o644))

		res := e.readExec(t.Context(), `{"path":"cr.txt"}`)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "\r") // a lone \r is preserved, unlike CRLF
	})
}

// Serial: flips the package-wide images toggle.
func TestReadImageGate(t *testing.T) {
	SetImagesEnabled(false)
	t.Cleanup(func() { SetImagesEnabled(true) })

	e := newToolEnv(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "pic.png"), imagePNG(t), 0o644))
	c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"pic.png"}`)}
	tool := &readTool{policy: e.policy, tracker: e.tracker, vision: func() bool { return true }}
	res, _ := tool.Execute(t.Context(), c, agent.NewOutput(agent.NopSink{}, "c"))

	// the note landed and vouches while images stay disabled
	require.False(t, res.IsError)
	assert.Equal(t, imagesDisabledText, textOf(res))
	assert.True(t, e.tracker.Unchanged(filepath.Join(e.cwd, "pic.png")))

	// gate on: the note no longer vouches, the next @img re-injects the picture
	SetImagesEnabled(true)
	assert.False(t, e.tracker.Unchanged(filepath.Join(e.cwd, "pic.png")))
}

// TestReadByteBoundNoDedupe narrows the shared read limit, so it runs in the
// sequential phase rather than parallel with every limit reader.
func TestReadByteBoundNoDedupe(t *testing.T) {
	applyTestLimits(t, Limits{Read: Limit{Bytes: 24}})

	// a byte-bound cut is partial even at the default window: what landed stops
	// short of the file's end
	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "1111111111\n2222222222\n3333333333\n4444444444\n")
	path := filepath.Join(e.cwd, "a.txt")

	res := e.readExec(t.Context(), `{"path":"a.txt"}`)
	assert.False(t, res.IsError)
	assert.Contains(t, textOf(res), "... truncated at line") // the window was cut
	assert.False(t, e.tracker.Unchanged(path))
}
