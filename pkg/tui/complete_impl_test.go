package tui

import (
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommonPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		texts []string
		want  string
	}{
		{"no_items", nil, ""},
		{"single_item", []string{"docs/"}, "docs/"},
		{"shared_prefix", []string{"docs/", "docs2/"}, "docs"},
		{"no_overlap", []string{"docs/", "pkg/"}, ""},
		{"one_is_prefix", []string{"main.go", "main.go.bak"}, "main.go"},
		{"case_sensitive", []string{"Makefile", "main.go"}, ""},
		{"grapheme_boundary", []string{"éclair.go", "écho.go"}, "éc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := make([]Completion, len(c.texts))
			for i, s := range c.texts {
				items[i] = Completion{Text: s}
			}
			assert.Equal(t, c.want, commonPrefix(items))
		})
	}
}

func TestCompletionOverlayRows(t *testing.T) {
	t.Parallel()

	theme := NewTheme(ColorNone, DefaultPalette())

	// bare labels pack into columns like a shell listing
	t.Run("packs_into_columns", func(t *testing.T) {
		l := &completionOverlay{items: itemsOf("aa", "bb", "cc", "dd")}

		rows := l.rows(theme, 12, 4)
		assert.Equal(t, []string{"  aa  bb", "  cc  dd"}, rows)
	})

	// a narrow terminal still renders one candidate per row
	t.Run("single_column_when_narrow", func(t *testing.T) {
		l := &completionOverlay{items: itemsOf("aaaa", "bbbb")}

		rows := l.rows(theme, 8, 4)
		assert.Equal(t, []string{"  aaaa", "  bbbb"}, rows)
	})

	// overflow reserves its last row for the count of what was dropped
	t.Run("overflow_counts_remainder", func(t *testing.T) {
		l := &completionOverlay{items: itemsOf("aa", "bb", "cc", "dd", "ee", "ff")}

		rows := l.rows(theme, 12, 2)
		require.Len(t, rows, 2)
		assert.Equal(t, "  aa  bb", rows[0])
		assert.Contains(t, rows[1], "4 more")
	})

	// detail text forces one candidate per row
	t.Run("detail_forces_one_per_row", func(t *testing.T) {
		l := &completionOverlay{items: []Completion{
			{Label: "/model", Detail: "switch model"},
			{Label: "/memory"},
		}}

		rows := l.rows(theme, 40, 4)
		require.Len(t, rows, 2)
		assert.Equal(t, "  /model  switch model", rows[0])
		assert.Equal(t, "  /memory", rows[1])
	})

	// a menu is one per row and marks its highlight, never packed into columns
	t.Run("menu_marks_the_highlight", func(t *testing.T) {
		l := &completionOverlay{menu: true, items: itemsOf("aa", "bb"), cursor: 1}

		rows := l.rows(theme, 40, 4)
		assert.Equal(t, []string{"  aa", "> bb"}, rows)
	})
}

func itemsOf(labels ...string) []Completion {
	return bulk.SliceTransform(func(s string) Completion {
		return Completion{Text: s, Label: s}
	}, labels)
}
