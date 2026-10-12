package command

import "strings"

// Kind classifies a submitted line so dispatch has one place to decide.
type Kind uint8

const (
	KindPrompt  Kind = iota // a normal message for the model
	KindCommand             // a /slash command
	KindShell               // a !shell command
)

// Line is a classified submitted line.
type Line struct {
	Kind Kind
	Rest string // the line after escape handling and prefix, for commands the name+args
	// Excluded marks a `!!` shell run: executed and displayed but kept out of
	// model context and the transcript.
	Excluded bool
	// Multiline marks a multi-line paste opening with `/`: sent as one verbatim
	// prompt instead of a command dispatch consuming the whole submission.
	Multiline bool
}

// ParseLine classifies a submitted line: `//` and leading space escape to a
// literal prompt, `/name args` is a command, `!cmd` stages a shell command,
// `!!cmd` runs one excluded from model context, else a prompt. Unknown `/foo`
// still parses as a command so dispatch can notice typos.
//
// A multi-line submission opening with `/name args` falls back to a verbatim
// prompt (Multiline set): pasted content may open with any prefix and one
// command dispatch must not consume it. Multi-line `!` keeps its staged run.
func ParseLine(s string) Line {
	// a leading space escapes anything: the line is a literal prompt
	if strings.HasPrefix(s, " ") {
		return Line{Kind: KindPrompt, Rest: s[1:]}
	}
	// `//x` => prompt `/x`
	if strings.HasPrefix(s, "//") {
		return Line{Kind: KindPrompt, Rest: s[1:]}
	}
	// `/name args` never spans lines: a paste opening with one is sent as a
	// verbatim prompt (Multiline flags the fallback) so nothing is consumed.
	// Multi-line `!` keeps staging below, user-initiated like any staged run.
	if strings.HasPrefix(s, "/") && strings.Contains(s, "\n") {
		return Line{Kind: KindPrompt, Rest: s, Multiline: true}
	}
	// `!!x` runs x but keeps it out of context, `!x` stages x onto context
	if strings.HasPrefix(s, "!!") {
		return Line{Kind: KindShell, Rest: s[2:], Excluded: true}
	}
	if strings.HasPrefix(s, "!") {
		return Line{Kind: KindShell, Rest: s[1:]}
	}
	if strings.HasPrefix(s, "/") {
		return Line{Kind: KindCommand, Rest: s[1:]}
	}
	return Line{Kind: KindPrompt, Rest: s}
}

// SplitCommand splits a command line's rest into name and argument. The name is
// lowercased, while the argument keeps its original case, trimmed of surrounding
// whitespace. An empty name (e.g. a bare `/`) classifies as no command.
func SplitCommand(rest string) (name, arg string, ok bool) {
	name, arg, _ = strings.Cut(rest, " ")
	name = strings.ToLower(name)
	arg = strings.TrimSpace(arg)
	return name, arg, name != ""
}
