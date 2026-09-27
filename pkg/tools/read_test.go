package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		_, ok := e.tracker.Records()[filepath.Join(e.cwd, "a.txt")]
		assert.True(t, ok) // read records the file so @ref expansion can dedupe
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
