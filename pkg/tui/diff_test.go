package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderDiff(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

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
		assert.Contains(t, out, "\n-a\n")
		assert.Contains(t, out, "\n+b\n")
	})
	t.Run("keeps_hunk_and_context", func(t *testing.T) {
		out := RenderDiff(plain, "x.go", "a\nb\nc\n", "a\nB\nc\n")
		assert.Contains(t, out, "@@")
		assert.Contains(t, out, "\n a\n")
	})
	t.Run("intraline_emphasis_applied", func(t *testing.T) {
		th := NewTheme(Color256)
		out := RenderDiff(th, "x.go", "value := compute(a)\n", "value := compute(a, b)\n")
		assert.Contains(t, out, th.DiffAddWord.Open())
	})
	t.Run("dissimilar_lines_not_emphasized", func(t *testing.T) {
		th := NewTheme(Color256)
		out := RenderDiff(th, "x.go", "aaaaaaaaaa\n", "zzzzzzzzzz\n")
		assert.NotContains(t, out, th.DiffAddWord.Open())
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
	t.Run("span_past_end_clamped", func(t *testing.T) {
		assert.NotPanics(t, func() {
			applySpans("+", "abc", [][2]int{{1, 99}}, base, emph)
		})
	})
}
