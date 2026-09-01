package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
)

// numbered builds the source of a file whose lines are "L1".."Ln".
func numberedFile(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString("L" + strconv.Itoa(i) + "\n")
	}
	return b.String()
}

func TestRenderDiff(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone, DefaultPalette())

	t.Run("identical_is_empty", func(t *testing.T) {
		assert.Empty(t, RenderDiff(plain, "x.go", "same\n", "same\n"))
	})
	t.Run("header_counts_changes", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\nb\nc\n", "a\nB\nc\nd\n")
		assert.True(t, strings.HasPrefix(out, "x.go +2 -1\n"))
	})
	t.Run("file_labels_dropped", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\n", "b\n")
		assert.NotContains(t, out, "--- x.go")
		assert.NotContains(t, out, "+++ x.go")
	})
	t.Run("marks_added_and_removed", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\n", "b\n")
		assert.Contains(t, out, "\n 1 - a\n")
		assert.Contains(t, out, "\n 1 + b\n")
	})
	t.Run("keeps_only_modest_context", func(t *testing.T) {
		before := numberedFile(20)
		after := strings.Replace(before, "L10\n", "TEN\n", 1)
		out := RenderDiff(plain, "x.go", before, after)

		assert.Contains(t, out, "@@ -7,7 +7,7 @@")
		assert.Contains(t, out, "\n10 - L10\n")
		assert.Contains(t, out, "\n10 + TEN\n")
		for _, want := range []string{"\n 7   L7\n", "\n 9   L9\n", "\n11   L11\n", "\n13   L13\n"} {
			assert.Contains(t, out, want)
		}
		// the rest of the file is not reprinted around the change
		for _, skip := range []string{"L1\n", "\n 6   L6\n", "\n14   L14\n", "L20"} {
			assert.NotContains(t, out, skip)
		}
	})
	t.Run("separate_changes_get_separate_hunks", func(t *testing.T) {
		before := numberedFile(40)
		after := strings.Replace(before, "L5\n", "FIVE\n", 1)
		after = strings.Replace(after, "L30\n", "THIRTY\n", 1)
		out := RenderDiff(plain, "x.go", before, after)

		assert.Contains(t, out, "@@ -2,7 +2,7 @@")
		assert.Contains(t, out, "@@ -27,7 +27,7 @@")
		assert.NotContains(t, out, "L18") // the untouched middle is skipped entirely
	})
	t.Run("hunk_header_drops_a_single_line_count", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\n", "b\n")
		assert.Contains(t, out, "@@ -1 +1 @@")
	})
	t.Run("new_file_is_all_additions", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "", "a\nb\n")
		assert.True(t, strings.HasPrefix(out, "x.go +2 -0\n"))
		assert.Contains(t, out, "@@ -0,0 +1,2 @@")
		assert.Contains(t, out, "\n 1 + a\n")
		assert.Contains(t, out, "\n 2 + b\n")
	})
	t.Run("gutter_widens_with_line_count", func(t *testing.T) {
		before := numberedFile(120)
		after := strings.Replace(before, "L100\n", "HUNDRED\n", 1)
		out := RenderDiff(plain, "x.go", before, after)
		assert.Contains(t, out, "\n 97   L97\n")  // right aligned in a 3 wide gutter
		assert.Contains(t, out, "\n100 - L100\n") // deletions carry the old number
		assert.Contains(t, out, "\n100 + HUNDRED\n")
	})
	t.Run("numbers_follow_the_new_file_after_a_shift", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\nb\n", "a\nx\ny\nb\n")
		assert.Contains(t, out, "\n 1   a\n")
		assert.Contains(t, out, "\n 2 + x\n")
		assert.Contains(t, out, "\n 3 + y\n")
		assert.Contains(t, out, "\n 4   b\n") // context renumbered, not left at 2
	})
	t.Run("tabs_expanded", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "\ta\n", "\tb\n")
		assert.NotContains(t, out, "\t")
		assert.Contains(t, out, "\n 1 - "+tabSpaces+"a\n")
	})
	t.Run("plain_theme_has_no_escapes", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "aaaa\n", "aaab\n")
		assert.NotContains(t, out, "\x1b")
	})
	t.Run("gutter_stays_plain_before_marker", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		out := RenderDiff(th, "x.go", "aaaa\n", "aaab\n")
		// the line number gutter renders in the default foreground: a changed row's
		// first escape sequence comes after its digits and space, at the marker.
		delLine := strings.Split(out, "\n")[2] // header, @@ hunk, then rows
		assert.True(t, strings.HasPrefix(delLine, " 1 "), delLine)
		_, rest, _ := strings.Cut(delLine, th.DiffDel.Open())
		assert.True(t, strings.HasPrefix(rest, "- a"), delLine)
	})
	t.Run("intraline_emphasis_applied", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		out := RenderDiff(th, "x.go", "value := compute(a)\n", "value := compute(a, b)\n")
		assert.Contains(t, out, th.DiffAddWord.Open())
	})
	t.Run("dissimilar_lines_not_emphasized", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		out := RenderDiff(th, "x.go", "aaaaaaaaaa\n", "zzzzzzzzzz\n")
		assert.NotContains(t, out, th.DiffAddWord.Open())
	})
	t.Run("no_background_shading", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		out := RenderDiff(th, "x.go", "short\nsecond\n", "a much longer line\nsecond\n")
		assert.NotContains(t, out, "\x1b[48;") // no background SGR anywhere
		// and no padding: the short side keeps its own length
		assert.Contains(t, strutil.StripANSI(out), "\n 1 - short\n")
		assert.Contains(t, strutil.StripANSI(out), "\n 2   second\n")
	})
}

func TestDiffSummary(t *testing.T) {
	t.Parallel()

	t.Run("counts_both_sides", func(t *testing.T) {
		assert.Equal(t, "x.go +2 -1 (shown above)",
			DiffSummary("x.go", "a\nb\nc\n", "a\nB\nc\nd\n"))
	})
	t.Run("new_file_is_all_additions", func(t *testing.T) {
		assert.Equal(t, "x.go +2 -0 (shown above)", DiffSummary("x.go", "", "a\nb\n"))
	})
	t.Run("identical_is_empty", func(t *testing.T) {
		assert.Empty(t, DiffSummary("x.go", "same\n", "same\n"))
	})
	t.Run("matches_render_header", func(t *testing.T) {
		before, after := numberedFile(20), strings.Replace(numberedFile(20), "L10\n", "TEN\n", 1)
		header, _, _ := strings.Cut(RenderDiff(NewTheme(ColorNone, DefaultPalette()), "x.go", before, after), "\n")
		assert.Equal(t, header+" (shown above)", DiffSummary("x.go", before, after))
	})
}

func TestIntralineSpans(t *testing.T) {
	t.Parallel()

	t.Run("identical_no_spans", func(t *testing.T) {
		del, add := intralineSpans("abc", "abc")
		assert.Nil(t, del)
		assert.Nil(t, add)
	})
	t.Run("insertion_marks_new_only", func(t *testing.T) {
		del, add := intralineSpans("call()", "call(ctx)")
		assert.Empty(t, del)
		assert.Equal(t, [][2]int{{5, 8}}, add)
	})
	t.Run("deletion_marks_old_only", func(t *testing.T) {
		del, add := intralineSpans("call(ctx)", "call()")
		assert.Equal(t, [][2]int{{5, 8}}, del)
		assert.Empty(t, add)
	})
	t.Run("too_dissimilar_returns_nil", func(t *testing.T) {
		del, add := intralineSpans("aaaaaaaa", "bbbbbbbb")
		assert.Nil(t, del)
		assert.Nil(t, add)
	})
	t.Run("spans_within_bounds", func(t *testing.T) {
		before, after := "func retry(n int) error {", "func retry(ctx context.Context, n int) error {"
		del, add := intralineSpans(before, after)
		for _, s := range del {
			assert.LessOrEqual(t, s[1], len(before))
		}
		for _, s := range add {
			assert.LessOrEqual(t, s[1], len(after))
		}
	})
}

func TestMergeSpans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    [][2]int
		expected [][2]int
	}{
		{"empty", nil, nil},
		{"single", [][2]int{{1, 3}}, [][2]int{{1, 3}}},
		{"close_merged", [][2]int{{1, 3}, {5, 8}}, [][2]int{{1, 8}}},
		{"far_kept", [][2]int{{1, 3}, {20, 25}}, [][2]int{{1, 3}, {20, 25}}},
		{"chain_merged", [][2]int{{0, 2}, {3, 5}, {6, 9}}, [][2]int{{0, 9}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, mergeSpans(tc.input))
		})
	}
}

func TestApplySpans(t *testing.T) {
	t.Parallel()

	base := Style{open: sgr(attrFgGreen)}
	emph := Style{open: sgr(attrReverse)}

	t.Run("no_spans_wraps_whole", func(t *testing.T) {
		assert.Equal(t, "\x1b[32m+abc\x1b[0m", applySpans("+", "abc", nil, base, emph))
	})
	t.Run("plain_theme_passthrough", func(t *testing.T) {
		var none Style
		assert.Equal(t, "+abc", applySpans("+", "abc", [][2]int{{1, 2}}, none, none))
	})
	t.Run("emphasizes_span", func(t *testing.T) {
		out := applySpans("+", "abc", [][2]int{{1, 2}}, base, emph)
		assert.Equal(t, "\x1b[32m+a\x1b[7mb\x1b[0m\x1b[32mc\x1b[0m", out)
	})
}
