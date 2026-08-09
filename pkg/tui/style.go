package tui

import "strings"

// SGR attribute codes used to build the palette
const (
	attrBold      = 1
	attrDim       = 2
	attrItalic    = 3
	attrReverse   = 7
	attrStrike    = 9
	attrFgRed     = 31
	attrFgGreen   = 32
	attrFgYellow  = 33
	attrFgBlue    = 34
	attrFgMagenta = 35
	attrFgCyan    = 36
)

// ColorProfile is the level of color support detected for the output terminal.
type ColorProfile int

const (
	ColorNone ColorProfile = iota
	ColorBasic
	Color256
	ColorTrue
)

// DetectColorProfile returns the profile implied by env lookups and whether output is a terminal.
func DetectColorProfile(env func(string) string, isTTY bool) ColorProfile {
	if !isTTY || env("NO_COLOR") != "" {
		return ColorNone
	}
	term := env("TERM")
	if term == "" || term == "dumb" {
		return ColorNone
	}
	switch strings.ToLower(env("COLORTERM")) {
	case "truecolor", "24bit":
		return ColorTrue
	}
	if strings.Contains(term, "256") || strings.Contains(term, "direct") {
		return Color256
	}
	return ColorBasic
}

// Style wraps text in SGR sequences. The zero value renders text unchanged.
type Style struct {
	open string
}

// Wrap returns text surrounded by the style's escape sequences.
func (s Style) Wrap(text string) string {
	if s.open == "" || text == "" {
		return text
	}
	return s.open + text + sgrReset
}

// Open returns the sequence enabling the style, empty when the style is a no-op.
func (s Style) Open() string { return s.open }

// Close returns the reset sequence, empty when the style is a no-op.
func (s Style) Close() string {
	if s.open == "" {
		return ""
	}
	return sgrReset
}

// Theme is the full palette. Every field is safe to use when color is disabled.
type Theme struct {
	Profile ColorProfile

	Thinking Style // shaded so it reads as internal, not output
	User     Style
	Prompt   Style
	Dim      Style
	Accent   Style
	Heading  Style
	Bold     Style
	Italic   Style
	Strike   Style
	Code     Style
	Link     Style
	Quote    Style
	Spinner  Style

	DiffAdd     Style
	DiffDel     Style
	DiffHunk    Style
	DiffFile    Style
	DiffAddWord Style
	DiffDelWord Style
}

// NewTheme returns the palette for the given color profile.
func NewTheme(p ColorProfile) Theme {
	t := Theme{Profile: p}
	if p == ColorNone {
		return t
	}
	style := func(params ...int) Style { return Style{open: sgr(params...)} }
	fg := func(n256, basic int) []int {
		if p >= Color256 {
			return []int{38, 5, n256}
		}
		return []int{basic}
	}
	styleFg := func(n256, basic int, extra ...int) Style {
		return style(append(extra, fg(n256, basic)...)...)
	}

	t.Thinking = styleFg(245, attrFgBlue, attrDim, attrItalic)
	t.User = styleFg(75, attrFgCyan, attrBold)
	t.Prompt = styleFg(75, attrFgCyan, attrBold)
	t.Dim = style(attrDim)
	t.Accent = styleFg(213, attrFgMagenta)
	t.Heading = styleFg(111, attrFgBlue, attrBold)
	t.Bold = style(attrBold)
	t.Italic = style(attrItalic)
	t.Strike = style(attrStrike)
	t.Code = styleFg(180, attrFgYellow)
	t.Link = styleFg(110, attrFgBlue)
	t.Quote = style(attrDim, attrItalic)
	t.Spinner = styleFg(213, attrFgMagenta)

	t.DiffAdd = styleFg(78, attrFgGreen)
	t.DiffDel = styleFg(167, attrFgRed)
	t.DiffHunk = styleFg(245, attrFgCyan, attrDim)
	t.DiffFile = styleFg(111, attrFgBlue, attrBold)
	t.DiffAddWord = style(append([]int{attrReverse}, fg(78, attrFgGreen)...)...)
	t.DiffDelWord = style(append([]int{attrReverse}, fg(167, attrFgRed)...)...)
	return t
}
