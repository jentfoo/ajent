package tui

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-analyze/bulk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectColorProfile(t *testing.T) {
	t.Parallel()

	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	tests := []struct {
		name     string
		vars     map[string]string
		isTTY    bool
		expected ColorProfile
	}{
		{"not_a_tty", map[string]string{"TERM": "xterm-256color"}, false, ColorNone},
		{"no_color_set", map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, true, ColorNone},
		{"dumb_term", map[string]string{"TERM": "dumb"}, true, ColorNone},
		{"empty_term", map[string]string{}, true, ColorNone},
		{"truecolor", map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}, true, ColorTrue},
		{"24bit", map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, true, ColorTrue},
		{"term_256", map[string]string{"TERM": "screen-256color"}, true, Color256},
		{"basic_only", map[string]string{"TERM": "xterm"}, true, ColorBasic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, DetectColorProfile(env(tc.vars), tc.isTTY))
		})
	}
}

func TestStyleWrap(t *testing.T) {
	t.Parallel()

	t.Run("zero_value_passthrough", func(t *testing.T) {
		var s Style
		assert.Equal(t, "hello", s.Wrap("hello"))
		assert.Empty(t, s.Open())
		assert.Empty(t, s.Close())
	})
	t.Run("wraps_with_reset", func(t *testing.T) {
		s := Style{open: sgr(attrBold)}
		assert.Equal(t, "\x1b[1mhello\x1b[0m", s.Wrap("hello"))
		assert.Equal(t, sgrReset, s.Close())
	})
	t.Run("empty_text_unchanged", func(t *testing.T) {
		s := Style{open: sgr(attrBold)}
		assert.Empty(t, s.Wrap(""))
	})
}

func TestNewTheme(t *testing.T) {
	t.Parallel()

	t.Run("color_none_is_noop", func(t *testing.T) {
		th := NewTheme(ColorNone, DefaultPalette())
		assert.Equal(t, "text", th.Thinking.Wrap("text"))
		assert.Equal(t, "text", th.DiffAdd.Wrap("text"))
	})
	t.Run("basic_uses_16_color", func(t *testing.T) {
		th := NewTheme(ColorBasic, DefaultPalette())
		assert.Equal(t, "\x1b[32m+ok\x1b[0m", th.DiffAdd.Wrap("+ok"))
	})
	t.Run("256_uses_extended", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		assert.Equal(t, "\x1b[38;5;78m+ok\x1b[0m", th.DiffAdd.Wrap("+ok"))
		assert.Equal(t, "\x1b[2;3;38;5;245mhmm\x1b[0m", th.Thinking.Wrap("hmm"))
	})
	t.Run("activity_shades_background_at_256", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		assert.Equal(t, "\x1b[2;48;5;236mrow\x1b[0m", th.Activity.Wrap("row"))
	})
	t.Run("activity_falls_back_to_dim_at_basic", func(t *testing.T) {
		assert.Equal(t, "\x1b[2mrow\x1b[0m", NewTheme(ColorBasic, DefaultPalette()).Activity.Wrap("row"))
	})
	t.Run("zero_palette_is_default", func(t *testing.T) {
		assert.Equal(t, NewTheme(Color256, DefaultPalette()), NewTheme(Color256, Palette{}))
	})
	t.Run("color_none_ignores_palette", func(t *testing.T) {
		for _, pal := range Palettes() {
			th := NewTheme(ColorNone, pal)
			assert.Equal(t, pal.Name, th.Palette)
			assert.Equal(t, "text", th.Accent.Wrap("text"))
			assert.Equal(t, "text", th.Activity.Wrap("text"))
		}
	})
	// the dark palette is what every existing user sees; it must not drift
	t.Run("dark_palette_unchanged", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		expected := map[string]string{
			"thinking": "\x1b[2;3;38;5;245m", "user": "\x1b[1;38;5;75m", "prompt": "\x1b[1;38;5;75m",
			"dim": "\x1b[2m", "accent": "\x1b[38;5;213m", "heading": "\x1b[1;38;5;111m",
			"bold": "\x1b[1m", "italic": "\x1b[3m", "strike": "\x1b[9m",
			"code": "\x1b[38;5;180m", "link": "\x1b[38;5;110m", "quote": "\x1b[2;3m",
			"spinner": "\x1b[38;5;213m", "divider": "\x1b[7m", "activity": "\x1b[2;48;5;236m",
			"userTag": "\x1b[38;5;69m", "assist": "\x1b[38;5;221m",
			"warn": "\x1b[38;5;179m", "error": "\x1b[38;5;167m",
			"diffAdd": "\x1b[38;5;78m", "diffDel": "\x1b[38;5;167m", "diffHunk": "\x1b[38;5;45m",
			"diffFile":    "\x1b[1;38;5;111m",
			"diffAddWord": "\x1b[7;38;5;78m", "diffDelWord": "\x1b[7;38;5;167m",
		}
		assert.Equal(t, expected, map[string]string{
			"thinking": th.Thinking.Open(), "user": th.User.Open(), "prompt": th.Prompt.Open(),
			"dim": th.Dim.Open(), "accent": th.Accent.Open(), "heading": th.Heading.Open(),
			"bold": th.Bold.Open(), "italic": th.Italic.Open(), "strike": th.Strike.Open(),
			"code": th.Code.Open(), "link": th.Link.Open(), "quote": th.Quote.Open(),
			"spinner": th.Spinner.Open(), "divider": th.Divider.Open(), "activity": th.Activity.Open(),
			"userTag": th.UserTag.Open(), "assist": th.Assist.Open(),
			"warn": th.Warn.Open(), "error": th.Error.Open(),
			"diffAdd": th.DiffAdd.Open(), "diffDel": th.DiffDel.Open(), "diffHunk": th.DiffHunk.Open(),
			"diffFile":    th.DiffFile.Open(),
			"diffAddWord": th.DiffAddWord.Open(), "diffDelWord": th.DiffDelWord.Open(),
		})
	})
}

func TestPalettes(t *testing.T) {
	t.Parallel()

	t.Run("hues_are_pinned", func(t *testing.T) {
		// indexes in role order: thinking, user, accent, heading, code, link, warn,
		// error, diffAdd, diffDel, diffHunk, userTag, assist, activity background
		expected := map[string][]int{
			"dark":        {245, 75, 213, 111, 180, 110, 179, 167, 78, 167, 45, 69, 221, 236},
			"dark-cool":   {244, 81, 117, 75, 152, 110, 180, 174, 114, 174, 80, 81, 152, 236},
			"dark-warm":   {246, 216, 210, 179, 223, 109, 214, 203, 108, 203, 180, 216, 151, 235},
			"dark-muted":  {243, 109, 139, 103, 144, 109, 137, 131, 108, 131, 66, 103, 144, 237},
			"light":       {240, 25, 90, 20, 94, 26, 130, 124, 28, 124, 30, 26, 92, 253},
			"light-cool":  {241, 24, 61, 18, 66, 25, 94, 125, 29, 125, 24, 25, 60, 254},
			"light-warm":  {242, 88, 126, 52, 130, 25, 166, 160, 64, 160, 95, 88, 100, 230},
			"light-muted": {244, 60, 96, 59, 95, 60, 137, 131, 65, 131, 66, 60, 101, 252},
		}
		require.Len(t, Palettes(), len(expected))
		for _, pal := range Palettes() {
			th := NewTheme(Color256, pal)
			actual := []int{
				fgIndex(th.Thinking), fgIndex(th.User), fgIndex(th.Accent), fgIndex(th.Heading),
				fgIndex(th.Code), fgIndex(th.Link), fgIndex(th.Warn), fgIndex(th.Error),
				fgIndex(th.DiffAdd), fgIndex(th.DiffDel), fgIndex(th.DiffHunk),
				fgIndex(th.UserTag), fgIndex(th.Assist), bgIndex(th.Activity),
			}
			assert.Equal(t, expected[pal.Name], actual, pal.Name)
		}
	})
	t.Run("basic_fallbacks_are_readable", func(t *testing.T) {
		for _, pal := range Palettes() {
			th := NewTheme(ColorBasic, pal)
			for _, s := range []Style{th.Thinking, th.User, th.Accent, th.Heading, th.Code,
				th.Link, th.Warn, th.Error, th.DiffAdd, th.DiffDel, th.DiffHunk, th.UserTag, th.Assist} {
				code := basicCode(s)
				assert.GreaterOrEqual(t, code, attrFgRed, pal.Name)
				assert.LessOrEqual(t, code, attrFgCyan, pal.Name)
				if pal.Tone == ToneLight {
					assert.NotEqual(t, attrFgCyan, code, pal.Name) // cyan washes out on white
				}
			}
		}
	})
	t.Run("tones_are_balanced", func(t *testing.T) {
		dark, light := PalettesFor(ToneDark), PalettesFor(ToneLight)
		assert.Len(t, dark, 4)
		assert.Len(t, light, 4)
		assert.Equal(t, Palettes(), PalettesFor(ToneUnknown))
		assert.Equal(t, ToneDark, DefaultPalette().Tone)
	})
}

func TestPaletteInvariants(t *testing.T) {
	t.Parallel()

	// reflection over roleHues so a hue added later is checked in every palette
	// without editing this test: an unset field would otherwise zero-fill into
	// SGR "38;5;0" (black) or, worse, a bare "0" that resets rather than colors
	t.Run("every_hue_is_set", func(t *testing.T) {
		for _, pal := range Palettes() {
			hues := reflect.ValueOf(pal.hues)
			for i := range hues.NumField() {
				name := pal.Name + "." + hues.Type().Field(i).Name
				f := hues.Field(i)
				if f.Kind() == reflect.Int { // a background shade, not a role hue
					assert.GreaterOrEqual(t, int(f.Int()), 16, name)
					assert.LessOrEqual(t, int(f.Int()), 255, name)
					continue
				}
				n256, basic := int(f.Field(0).Int()), int(f.Field(1).Int())
				// below 16 the terminal picks the hue, which defeats the palette
				assert.GreaterOrEqual(t, n256, 16, name)
				assert.LessOrEqual(t, n256, 255, name)
				assert.GreaterOrEqual(t, basic, attrFgRed, name)
				assert.LessOrEqual(t, basic, attrFgCyan, name)
			}
		}
	})
	t.Run("names_are_unique", func(t *testing.T) {
		assert.Len(t, bulk.SliceToSetBy(func(p Palette) string { return p.Name }, palettes), len(palettes))
	})
	t.Run("every_palette_names_a_code_style", func(t *testing.T) {
		for _, pal := range Palettes() {
			assert.NotEmpty(t, pal.codeStyle, pal.Name)
			assert.Equal(t, pal.codeStyle, NewTheme(Color256, pal).CodeStyle, pal.Name)
		}
	})
	t.Run("no_code_style_without_color", func(t *testing.T) {
		assert.Empty(t, NewTheme(ColorNone, DefaultPalette()).CodeStyle)
	})
	t.Run("no_code_style_below_256", func(t *testing.T) {
		assert.Empty(t, NewTheme(ColorBasic, DefaultPalette()).CodeStyle)
	})
}

func TestLookupPalette(t *testing.T) {
	t.Parallel()

	t.Run("every_name_resolves", func(t *testing.T) {
		for _, pal := range Palettes() {
			found, ok := LookupPalette(pal.Name)
			assert.True(t, ok)
			assert.Equal(t, pal, found)
		}
	})
	t.Run("unknown_name", func(t *testing.T) {
		found, ok := LookupPalette("solarized")
		assert.False(t, ok)
		assert.Empty(t, found.Name)
	})
	t.Run("empty_name", func(t *testing.T) {
		_, ok := LookupPalette("")
		assert.False(t, ok)
	})
}

// fgIndex returns the 256-color foreground a style sets, -1 when it sets none.
func fgIndex(s Style) int { return colorIndex(s, "38;5;") }

// bgIndex returns the 256-color background a style sets, -1 when it sets none.
func bgIndex(s Style) int { return colorIndex(s, "48;5;") }

func colorIndex(s Style, prefix string) int {
	_, rest, ok := strings.Cut(s.Open(), prefix)
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSuffix(rest, "m"))
	if err != nil {
		return -1
	}
	return n
}

// basicCode returns the single ANSI color a style sets under ColorBasic.
func basicCode(s Style) int {
	params := strings.TrimSuffix(strings.TrimPrefix(s.Open(), csi), "m")
	fields := strings.Split(params, ";")
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return -1
	}
	return n
}
