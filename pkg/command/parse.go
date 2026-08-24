// Package command owns the single dispatch path for submitted lines: slash
// commands through a registry, `!` shell commands executed directly with their
// results staged onto context, and the `Console` view of the world commands
// share. Extensions and MCP servers register into the same registry.
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
	Rest string // the line after any escape handling and prefix; for commands, the name+args
	// Excluded marks a `!!` shell run: executed and displayed but kept out of
	// model context and the transcript.
	Excluded bool
}

// ParseLine classifies a submitted line: `//` and leading space escape to a
// literal prompt, `/name args` is a command, `!cmd` stages a shell command,
// `!!cmd` runs one excluded from model context, else a prompt. Unknown `/foo`
// still parses as a command so dispatch can notice typos.
func ParseLine(s string) Line {
	// a leading space escapes anything: the line is a literal prompt
	if strings.HasPrefix(s, " ") {
		return Line{Kind: KindPrompt, Rest: s[1:]}
	}
	// `//x` => prompt `/x`
	if strings.HasPrefix(s, "//") {
		return Line{Kind: KindPrompt, Rest: s[1:]}
	}
	// `!!x` runs x but keeps it out of context; `!x` stages x onto context
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
// lowercased; the argument keeps its original case, trimmed of surrounding
// whitespace. An empty name (e.g. a bare `/`) classifies as no command.
func SplitCommand(rest string) (name, arg string, ok bool) {
	name, arg, _ = strings.Cut(rest, " ")
	name = strings.ToLower(name)
	arg = strings.TrimSpace(arg)
	return name, arg, name != ""
}
