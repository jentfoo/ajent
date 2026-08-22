package tui

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHighlight(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256, DefaultPalette())

	t.Run("go_tokens_colored", func(t *testing.T) {
		rows := highlight(th, "go", "func f() int { return 1 }")
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0], esc)
		assert.Equal(t, "func f() int { return 1 }", strutil.StripANSI(rows[0]))
	})
	t.Run("bash_tokens_colored", func(t *testing.T) {
		rows := highlight(th, "bash", "echo \"hi\" | wc -l")
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0], esc)
		assert.Equal(t, "echo \"hi\" | wc -l", strutil.StripANSI(rows[0]))
	})
	t.Run("json_tokens_colored", func(t *testing.T) {
		rows := highlight(th, "json", "{\"a\": 1}")
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0], esc)
		assert.Equal(t, "{\"a\": 1}", strutil.StripANSI(rows[0]))
	})
	t.Run("language_aliases_resolve", func(t *testing.T) {
		aliases := map[string]string{
			"golang": "func f() {}",
			"Go":     "func f() {}",
			"sh":     "echo \"hi\"",
			"yml":    "key: value",
		}
		for lang, code := range aliases {
			assert.NotEmpty(t, highlight(th, lang, code), lang)
		}
	})

	t.Run("unknown_language_falls_back", func(t *testing.T) {
		assert.Nil(t, highlight(th, "zzznotalanguage", "x := 1"))
	})
	t.Run("no_language_falls_back", func(t *testing.T) {
		assert.Nil(t, highlight(th, "", "x := 1"))
	})
	t.Run("plaintext_falls_back", func(t *testing.T) {
		// a plaintext lexer colors nothing, so the flat code style reads better
		for _, lang := range []string{"text", "plaintext", "plain"} {
			assert.Nil(t, highlight(th, lang, "just some words"), lang)
		}
	})
	t.Run("unknown_style_falls_back", func(t *testing.T) {
		bogus := th
		bogus.CodeStyle = "zzznotastyle"
		assert.Nil(t, highlight(bogus, "go", "x := 1"))
	})
	t.Run("no_style_falls_back", func(t *testing.T) {
		assert.Nil(t, highlight(NewTheme(ColorNone, DefaultPalette()), "go", "x := 1"))
		assert.Nil(t, highlight(NewTheme(ColorBasic, DefaultPalette()), "go", "x := 1"))
	})

	t.Run("line_count_matches_source", func(t *testing.T) {
		// SplitTokensIntoLines and strings.Split must agree or codeBlock falls back,
		// so the cases where a token spans lines are the ones that matter
		for _, code := range []string{
			"a := 1\nb := 2",
			"a := 1\n\nb := 2",
			"s := `multi\nline\nraw`",
			"/* block\ncomment */\nx := 1",
			"s := \"unterminated",
		} {
			assert.Len(t, highlight(th, "go", code), strings.Count(code, "\n")+1, code)
		}
	})
	t.Run("no_newline_within_a_row", func(t *testing.T) {
		// a histLine is one terminal row; a break inside a styled span would corrupt
		// the layout and cannot be trimmed off afterwards
		for _, row := range highlight(th, "go", "s := `multi\nline`\nx := 1") {
			assert.NotContains(t, row, "\n")
		}
	})
	t.Run("no_background_emitted", func(t *testing.T) {
		// unterminated input lexes as Error, which several styles shade
		for _, pal := range Palettes() {
			rows := highlight(NewTheme(Color256, pal), "go", "s := \"unterminated")
			assert.NotContains(t, strings.Join(rows, ""), "48;", pal.Name)
		}
	})
	t.Run("whitespace_is_never_colored", func(t *testing.T) {
		// github paints whitespace against its own page background, which in a
		// terminal is invisible and pure escape bloat
		for _, pal := range Palettes() {
			rows := highlight(NewTheme(Color256, pal), "go", "func f() {\n\treturn\n}")
			require.Len(t, rows, 3)
			assert.True(t, strings.HasPrefix(rows[1], "\t"), pal.Name) // indent unstyled
			for _, row := range rows {
				assert.False(t, hasEmptySpan(row), pal.Name)
			}
		}
	})
	t.Run("truecolor_uses_rgb", func(t *testing.T) {
		row := highlight(NewTheme(ColorTrue, DefaultPalette()), "go", "x := 1")[0]
		assert.Contains(t, row, "38;2;")
		assert.NotContains(t, row, "38;5;")
	})
	t.Run("every_palette_style_resolves", func(t *testing.T) {
		// a renamed chroma style would silently drop a palette back to flat code
		for _, pal := range Palettes() {
			assert.NotEmpty(t, highlight(NewTheme(Color256, pal), "go", "func f() {}"), pal.Name)
		}
	})
}

// hasEmptySpan reports a style opened and closed with no text between it, the
// artifact a colored whitespace or newline token leaves behind.
func hasEmptySpan(row string) bool {
	segs := splitANSI(row)
	for i := 0; i+1 < len(segs); i++ {
		if segs[i].escape && segs[i].text != sgrReset && segs[i+1].escape && segs[i+1].text == sgrReset {
			return true
		}
	}
	return false
}
