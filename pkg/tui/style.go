package tui

import (
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
)

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

// Tone is the terminal background a palette is built for.
type Tone int

const (
	ToneUnknown Tone = iota
	ToneDark
	ToneLight
)

// hue is one role's color: a 256 index with its 16-color fallback.
type hue struct{ n256, basic int }

// roleHues is every color a palette chooses. Attributes (bold, dim, italic,
// reverse) are the same in every palette and stay in NewTheme.
type roleHues struct {
	thinking, user, accent, heading, code, link hue
	warn, err, diffAdd, diffDel, diffHunk       hue
	userTag, assist                             hue
	activityBG                                  int // background index behind activity rows
}

// Palette is one built-in color set.
type Palette struct {
	Name string
	Tone Tone

	// codeStyle names the syntax-highlighting style fenced code uses, matched to
	// this palette's tone. It is carried, never resolved, here: the highlight
	// build maps the name onto its own style registry, so pkg/tui stays free of
	// that dependency.
	codeStyle string
	hues      roleHues
}

// palettes are the built-in color sets in display order. The first dark and
// first light entry are the conservative defaults; the rest trade contrast for
// a calmer, warmer or lower-saturation feel.
var palettes = []Palette{
	{Name: "dark", Tone: ToneDark, codeStyle: "monokai", hues: roleHues{
		thinking: hue{245, attrFgBlue}, user: hue{75, attrFgCyan}, accent: hue{213, attrFgMagenta},
		heading: hue{111, attrFgBlue}, code: hue{180, attrFgYellow}, link: hue{110, attrFgBlue},
		warn: hue{179, attrFgYellow}, err: hue{167, attrFgRed},
		diffAdd: hue{78, attrFgGreen}, diffDel: hue{167, attrFgRed}, diffHunk: hue{45, attrFgCyan},
		userTag: hue{69, attrFgBlue}, assist: hue{221, attrFgYellow}, activityBG: 236,
	}},
	{Name: "dark-cool", Tone: ToneDark, codeStyle: "nord", hues: roleHues{
		thinking: hue{244, attrFgBlue}, user: hue{81, attrFgCyan}, accent: hue{117, attrFgCyan},
		heading: hue{75, attrFgBlue}, code: hue{152, attrFgCyan}, link: hue{110, attrFgBlue},
		warn: hue{180, attrFgYellow}, err: hue{174, attrFgRed},
		diffAdd: hue{114, attrFgGreen}, diffDel: hue{174, attrFgRed}, diffHunk: hue{80, attrFgCyan},
		userTag: hue{81, attrFgBlue}, assist: hue{152, attrFgCyan}, activityBG: 236,
	}},
	{Name: "dark-warm", Tone: ToneDark, codeStyle: "dracula", hues: roleHues{
		thinking: hue{246, attrFgYellow}, user: hue{216, attrFgYellow}, accent: hue{210, attrFgMagenta},
		heading: hue{179, attrFgYellow}, code: hue{223, attrFgYellow}, link: hue{109, attrFgBlue},
		warn: hue{214, attrFgYellow}, err: hue{203, attrFgRed},
		diffAdd: hue{108, attrFgGreen}, diffDel: hue{203, attrFgRed}, diffHunk: hue{180, attrFgYellow},
		userTag: hue{216, attrFgYellow}, assist: hue{151, attrFgGreen}, activityBG: 235,
	}},
	{Name: "dark-muted", Tone: ToneDark, codeStyle: "paraiso-dark", hues: roleHues{
		thinking: hue{243, attrFgBlue}, user: hue{109, attrFgCyan}, accent: hue{139, attrFgMagenta},
		heading: hue{103, attrFgBlue}, code: hue{144, attrFgYellow}, link: hue{109, attrFgBlue},
		warn: hue{137, attrFgYellow}, err: hue{131, attrFgRed},
		diffAdd: hue{108, attrFgGreen}, diffDel: hue{131, attrFgRed}, diffHunk: hue{66, attrFgCyan},
		userTag: hue{103, attrFgBlue}, assist: hue{144, attrFgYellow}, activityBG: 237,
	}},
	// light fallbacks avoid basic cyan, which washes out on a white background;
	// warn keeps basic yellow because light themes render it as a dark olive
	{Name: "light", Tone: ToneLight, codeStyle: "github", hues: roleHues{
		thinking: hue{240, attrFgBlue}, user: hue{25, attrFgBlue}, accent: hue{90, attrFgMagenta},
		heading: hue{20, attrFgBlue}, code: hue{94, attrFgMagenta}, link: hue{26, attrFgBlue},
		warn: hue{130, attrFgYellow}, err: hue{124, attrFgRed},
		diffAdd: hue{28, attrFgGreen}, diffDel: hue{124, attrFgRed}, diffHunk: hue{30, attrFgMagenta},
		userTag: hue{26, attrFgBlue}, assist: hue{92, attrFgMagenta}, activityBG: 253,
	}},
	{Name: "light-cool", Tone: ToneLight, codeStyle: "solarized-light", hues: roleHues{
		thinking: hue{241, attrFgBlue}, user: hue{24, attrFgBlue}, accent: hue{61, attrFgMagenta},
		heading: hue{18, attrFgBlue}, code: hue{66, attrFgMagenta}, link: hue{25, attrFgBlue},
		warn: hue{94, attrFgYellow}, err: hue{125, attrFgRed},
		diffAdd: hue{29, attrFgGreen}, diffDel: hue{125, attrFgRed}, diffHunk: hue{24, attrFgMagenta},
		userTag: hue{25, attrFgBlue}, assist: hue{60, attrFgMagenta}, activityBG: 254,
	}},
	{Name: "light-warm", Tone: ToneLight, codeStyle: "autumn", hues: roleHues{
		thinking: hue{242, attrFgYellow}, user: hue{88, attrFgRed}, accent: hue{126, attrFgMagenta},
		heading: hue{52, attrFgRed}, code: hue{130, attrFgMagenta}, link: hue{25, attrFgBlue},
		warn: hue{166, attrFgYellow}, err: hue{160, attrFgRed},
		diffAdd: hue{64, attrFgGreen}, diffDel: hue{160, attrFgRed}, diffHunk: hue{95, attrFgMagenta},
		userTag: hue{88, attrFgRed}, assist: hue{100, attrFgYellow}, activityBG: 230,
	}},
	{Name: "light-muted", Tone: ToneLight, codeStyle: "friendly", hues: roleHues{
		thinking: hue{244, attrFgBlue}, user: hue{60, attrFgBlue}, accent: hue{96, attrFgMagenta},
		heading: hue{59, attrFgBlue}, code: hue{95, attrFgMagenta}, link: hue{60, attrFgBlue},
		warn: hue{137, attrFgYellow}, err: hue{131, attrFgRed},
		diffAdd: hue{65, attrFgGreen}, diffDel: hue{131, attrFgRed}, diffHunk: hue{66, attrFgMagenta},
		userTag: hue{60, attrFgBlue}, assist: hue{101, attrFgYellow}, activityBG: 252,
	}},
}

// Palettes returns the built-in palettes in display order.
func Palettes() []Palette { return slices.Clone(palettes) }

// PalettesFor returns the palettes built for tone, or all of them when tone is
// ToneUnknown.
func PalettesFor(tone Tone) []Palette {
	if tone == ToneUnknown {
		return Palettes()
	}
	return bulk.SliceFilter(func(p Palette) bool { return p.Tone == tone }, palettes)
}

// LookupPalette returns the palette with the given name, false when unknown.
func LookupPalette(name string) (Palette, bool) {
	i := slices.IndexFunc(palettes, func(p Palette) bool { return p.Name == name })
	if i < 0 {
		return Palette{}, false
	}
	return palettes[i], true
}

// DefaultPalette returns the palette used when configuration names none.
func DefaultPalette() Palette { return palettes[0] }

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
	Palette string // the built-in palette the hues came from
	// CodeStyle names the syntax-highlighting style for fenced code, empty when
	// color is disabled so a highlighter has nothing to apply.
	CodeStyle string

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
	Divider  Style // full-width solid band marking restored-context boundaries
	Activity Style // live sub-agent status rows: dim on a subtle background
	UserTag  Style // "user:" role tag in the rewind tree picker
	Assist   Style // "assistant:" role tag in the rewind tree picker
	Warn     Style // notice levels, kept separate from the diff palette
	Error    Style

	DiffAdd     Style
	DiffDel     Style
	DiffHunk    Style
	DiffFile    Style
	DiffAddWord Style
	DiffDelWord Style
}

// NewTheme returns the palette's roles rendered for the given color profile. A
// zero Palette resolves to DefaultPalette.
func NewTheme(p ColorProfile, pal Palette) Theme {
	if pal.Name == "" {
		pal = DefaultPalette()
	}
	t := Theme{Profile: p, Palette: pal.Name}
	if p == ColorNone {
		return t
	}
	t.CodeStyle = pal.codeStyle
	h := pal.hues
	style := func(params ...int) Style { return Style{open: sgr(params...)} }
	fg := func(c hue) []int {
		if p >= Color256 {
			return []int{38, 5, c.n256}
		}
		return []int{c.basic}
	}
	styleFg := func(c hue, extra ...int) Style {
		return style(append(extra, fg(c)...)...)
	}

	t.Thinking = styleFg(h.thinking, attrDim, attrItalic)
	t.User = styleFg(h.user, attrBold)
	t.Prompt = styleFg(h.user, attrBold)
	t.Dim = style(attrDim)
	t.Accent = styleFg(h.accent)
	t.Heading = styleFg(h.heading, attrBold)
	t.Bold = style(attrBold)
	t.Italic = style(attrItalic)
	t.Strike = style(attrStrike)
	t.Code = styleFg(h.code)
	t.Link = styleFg(h.link)
	t.Quote = style(attrDim, attrItalic)
	t.Spinner = styleFg(h.accent)
	// the divider is a solid full-width band; reverse video swaps default fg/bg
	// per cell into an inverted block that reads as thick and obvious in scrollback.
	t.Divider = style(attrReverse)
	// activity rows sit in the live block above the prompt; a soft background sets
	// them apart from committed output. Basic terminals lack a usable shade.
	if p >= Color256 {
		t.Activity = style(attrDim, 48, 5, h.activityBG)
	} else {
		t.Activity = style(attrDim)
	}
	t.Warn = styleFg(h.warn)
	t.Error = styleFg(h.err)

	t.DiffAdd = styleFg(h.diffAdd)
	t.DiffDel = styleFg(h.diffDel)
	// @@ range markers get their own hue, distinct from the add/del/context trio,
	// so a hunk boundary reads as a separator
	t.DiffHunk = styleFg(h.diffHunk)
	t.DiffFile = styleFg(h.heading, attrBold)
	t.UserTag = styleFg(h.userTag)
	t.Assist = styleFg(h.assist)
	t.DiffAddWord = styleFg(h.diffAdd, attrReverse)
	t.DiffDelWord = styleFg(h.diffDel, attrReverse)
	return t
}
