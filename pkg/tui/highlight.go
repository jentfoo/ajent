package tui

import (
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// caches keyed by chroma style name and by language. A nil value is a resolved
// miss, so an unknown name is not looked up again on the next block.
var (
	styleCache sync.Map // string -> *chroma.Style, defaults stripped
	lexerCache sync.Map // string -> chroma.Lexer, coalesced
)

// highlight returns one styled row per line of code, nil when the theme, language
// or color depth cannot produce one.
func highlight(t Theme, lang, code string) []string {
	if t.CodeStyle == "" || lang == "" {
		return nil
	}
	format := formatterFor(t.Profile)
	if format == nil {
		return nil
	}
	lexer := cachedLexer(lang)
	style := cachedStyle(t.CodeStyle)
	if lexer == nil || style == nil {
		return nil
	}
	it, err := lexer.Tokenise(nil, code)
	if err != nil {
		return nil
	}
	// one Format call for the whole block: the indexed formatter rebuilds its color
	// lookup table per call, which costs more than lexing when done per line
	var b strings.Builder
	if err := format.Format(&b, style, it); err != nil {
		return nil
	}
	out := b.String()
	if !strings.Contains(out, esc) { // a plaintext lexer colors nothing
		return nil
	}
	return splitStyledLines(out)
}

// splitStyledLines splits s at its line breaks into rows that each carry their own SGR.
func splitStyledLines(s string) []string {
	var out []string
	var b strings.Builder
	var active string
	for _, seg := range splitANSI(s) {
		if seg.escape {
			if seg.text == sgrReset {
				active = ""
			} else {
				active += seg.text
			}
			b.WriteString(seg.text)
			continue
		}
		for text := seg.text; ; {
			i := strings.IndexByte(text, '\n')
			if i < 0 {
				b.WriteString(text)
				break
			}
			b.WriteString(text[:i])
			if active != "" {
				b.WriteString(sgrReset)
			}
			out = append(out, b.String())
			b.Reset()
			b.WriteString(active)
			text = text[i+1:]
		}
	}
	return append(out, b.String())
}

// formatterFor returns the chroma formatter for the profile, nil below Color256.
func formatterFor(p ColorProfile) chroma.Formatter {
	switch p {
	case Color256:
		return formatters.TTY256
	case ColorTrue:
		return formatters.TTY16m
	default:
		return nil
	}
}

// cachedLexer returns the lexer for lang, nil when chroma has none.
func cachedLexer(lang string) chroma.Lexer {
	if v, ok := lexerCache.Load(lang); ok {
		l, _ := v.(chroma.Lexer)
		return l
	}
	var lexer chroma.Lexer
	if l := lexers.Get(lang); l != nil {
		lexer = chroma.Coalesce(l)
	}
	lexerCache.Store(lang, lexer)
	return lexer
}

// cachedStyle returns the named chroma style prepared for fenced code, nil when
// chroma has no such style.
func cachedStyle(name string) *chroma.Style {
	if v, ok := styleCache.Load(name); ok {
		s, _ := v.(*chroma.Style)
		return s
	}
	// styles.Get substitutes a fallback for an unknown name, which would silently
	// highlight in the wrong palette, so resolve against the registry directly
	style := styles.Registry[strings.ToLower(name)]
	if style != nil {
		style = stripDefaults(style)
	}
	styleCache.Store(name, style)
	return style
}

// stripDefaults returns s with token backgrounds and plain-text foregrounds
// cleared, leaving code at prose weight on the terminal background.
func stripDefaults(s *chroma.Style) *chroma.Style {
	base := s.Get(chroma.Text).Colour
	b := s.Builder().Transform(func(e chroma.StyleEntry) chroma.StyleEntry {
		e.Background = 0
		if e.Colour == base {
			e.Colour = 0
		}
		// a cleared entry reads as unset and resolves against the parent style, so
		// it has to refuse inheritance to stay cleared
		e.NoInherit = true
		return e
	})
	// prose and whitespace belong to the terminal's own foreground; whitespace has
	// no glyph to color at all once backgrounds are gone, and styles built for a
	// web page set it against theirs (github paints it white)
	blank := chroma.StyleEntry{NoInherit: true}
	built, err := b.AddEntry(chroma.Text, blank).AddEntry(chroma.TextWhitespace, blank).Build()
	if err != nil {
		return s
	}
	return built
}
