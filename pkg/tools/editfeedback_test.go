package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applied runs ops against content, failing if they do not apply.
func applied(t *testing.T, content string, ops ...editOp) editOutcome {
	t.Helper()
	o, err := applyEdits(editTarget{Path: "a.go"}, content, ops)
	require.NoError(t, err)
	return o
}

func TestUnifiedDiff(t *testing.T) {
	t.Parallel()

	t.Run("renders_change", func(t *testing.T) {
		d := unifiedDiff("a.go", "one\ntwo\nthree\n", "one\n2\nthree\n")
		assert.Contains(t, d, "--- a.go")
		assert.Contains(t, d, "@@")
		assert.Contains(t, d, "-two")
		assert.Contains(t, d, "+2")
	})

	t.Run("empty_when_equal", func(t *testing.T) {
		assert.Empty(t, unifiedDiff("a.go", "same\n", "same\n"))
	})

	t.Run("bounded", func(t *testing.T) {
		var before, after strings.Builder
		for i := 0; i < 4000; i++ {
			before.WriteString("old line\n")
			after.WriteString("new line\n")
		}
		d := unifiedDiff("a.go", before.String(), after.String())
		assert.Contains(t, d, "truncated")
		assert.Less(t, countLines(d), 500) // head and tail survive, the middle does not
	})
}

func TestEditReport(t *testing.T) {
	t.Parallel()

	t.Run("exact_match_carries_no_diff", func(t *testing.T) {
		o := applied(t, "one\ntwo\nthree\n", editOp{OldText: "two", NewText: "2"})
		out := editReport("a.go", 1, o)

		assert.Equal(t, "applied 1 edits to a.go", out) // it did exactly what was asked
	})

	t.Run("replace_all_reports_every_site", func(t *testing.T) {
		o := applied(t, "a\nx\nb\nx\n", editOp{OldText: "x", NewText: "y", ReplaceAll: true})
		out := editReport("a.go", 1, o)

		assert.Contains(t, out, "replaced 2 occurrences")
		assert.Contains(t, out, "-x")
		assert.Contains(t, out, "+y")
	})

	t.Run("duplicated_new_text_is_flagged", func(t *testing.T) {
		const block = "func helper() error { return nil }"
		o := applied(t, "package p\n\n"+block+"\n\nfunc other() {}\n",
			editOp{OldText: "func other() {}", NewText: block})
		out := editReport("a.go", 1, o)

		assert.Contains(t, out, "appears 2 times")
		assert.Contains(t, out, "@@") // the diff rides along so the model can look
	})

	t.Run("reemitted_line_is_flagged", func(t *testing.T) {
		// the old regression case: re-emitting a line the file kept
		o := applied(t, "A\nB\nC\nD\n", editOp{OldText: "C", NewText: "B\nC"})
		out := editReport("a.txt", 1, o)

		assert.Contains(t, out, "lines 2 and 3 are identical")
		assert.Contains(t, out, "@@")
	})

	t.Run("preexisting_adjacent_duplicate_not_flagged", func(t *testing.T) {
		o := applied(t, "keep\ndupe\ndupe\n", editOp{OldText: "keep", NewText: "kept"})
		assert.Empty(t, o.notes)
		assert.False(t, o.review)
	})

	t.Run("reindented_text_counts_as_written", func(t *testing.T) {
		// the indent tier writes at the file's indent, so counting raw newText
		// alone would miss a repeat of what was actually written
		const block = "fooBarBazQuxQuuxCorgeGrault(1)"
		src := "\tsomethingEntirelyDifferent(x)\n\t" + block + "\n"
		o := applied(t, src,
			editOp{OldText: "    somethingEntirelyDifferent(x)", NewText: "    " + block})
		out := editReport("a.go", 1, o)
		assert.Contains(t, out, "appears 2 times")
	})
}

func TestEditReportNamesANonExactMatch(t *testing.T) {
	t.Parallel()

	o := applied(t, "msg := \"hi\"\n", editOp{OldText: "msg := “hi”", NewText: "msg := \"bye\""})
	out := editReport("a.go", 1, o)

	assert.Contains(t, out, "edit 1")
	assert.Contains(t, out, "unicode characters")
	assert.Contains(t, out, `-msg := "hi"`) // the removed side names what the file held
}
