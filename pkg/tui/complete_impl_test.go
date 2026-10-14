package tui

import (
	"testing"

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
	out := make([]Completion, 0, len(labels))
	for _, s := range labels {
		out = append(out, Completion{Text: s, Label: s})
	}
	return out
}

func TestCompleteAt(t *testing.T) {
	t.Parallel()

	// a lone candidate fills in whole from the start of its span
	t.Run("single_candidate_fills", func(t *testing.T) {
		var ed editor
		ed.SetValue("@ma")
		ed.pos = 3

		o, changed := completeAt(&ed, 1, []Completion{{Text: "main.go"}}, false)

		assert.True(t, changed)
		assert.Nil(t, o)
		assert.Equal(t, "@main.go", ed.Value())
	})

	// ambiguity fills only the shared prefix
	t.Run("shared_prefix_only", func(t *testing.T) {
		var ed editor
		ed.SetValue("@mai")
		ed.pos = 4

		o, changed := completeAt(&ed, 1, []Completion{{Text: "main.go"}, {Text: "main_test.go"}}, false)

		assert.True(t, changed)
		assert.Nil(t, o)
		assert.Equal(t, "@main", ed.Value())
	})

	// nothing more to insert opens a listing over the candidates
	t.Run("ambiguous_opens_listing", func(t *testing.T) {
		var ed editor
		ed.SetValue("@main")
		ed.pos = 5
		items := []Completion{{Text: "main.go"}, {Text: "main_test.go"}}

		o, changed := completeAt(&ed, 1, items, false)

		assert.False(t, changed)
		require.NotNil(t, o)
		assert.Equal(t, items, o.items)
		assert.Equal(t, 1, o.start)
	})

	// acceptance is a programmatic fill: any pending kill snapshot must drop,
	// or Ctrl+Y restores an older buffer over the accepted text
	t.Run("accept_drops_kill_snapshot", func(t *testing.T) {
		var ed editor
		ed.SetValue("git st")
		ed.pos = 4
		ed.KillToLineEnd(60) // kills "st"

		o, changed := completeAt(&ed, 4, []Completion{{Text: "status"}}, false)

		assert.True(t, changed)
		assert.Nil(t, o)
		assert.Equal(t, "git status", ed.Value())
		assert.False(t, ed.Undo())
	})

	// a listing already open is kept: nothing to add and nothing new to show
	t.Run("open_listing_is_kept", func(t *testing.T) {
		var ed editor
		ed.SetValue("@main")
		ed.pos = 5

		o, changed := completeAt(&ed, 1, []Completion{{Text: "main.go"}, {Text: "main_test.go"}}, true)

		assert.False(t, changed)
		assert.Nil(t, o)
	})
}
