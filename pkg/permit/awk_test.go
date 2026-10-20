package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAwkReadSafe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// read-only scripts and flag forms stay allowed.
		{"print field", "awk '{print $1}' file", true},
		{`pattern compare`, `awk '$2 > 100' file`, true},
		{"field separator attach", "awk -F, '{print $1}' f", true},
		{"assignment value flag", "awk -v x=1 'BEGIN{print x}'", true},
		{"if condition print", "awk '{ if ($2 > 3) print $3 }' f", true},
		{"paren comparison", `awk '{print ($2 > 100)}'`, true},
		{"statement split", "awk '{print $1; if ($3 > 4) exit}'", true},
		{`string collapsed`, `awk 'BEGIN{print "a > b"}'`, true},

		// regex constants and division are not mistaken for strings or redirects.
		{"regex paren pattern", `awk '$0 ~ /\(/ {print $1}' f`, true},
		{"division not regex", `awk '{print $1 / 2}' f`, true},

		// exec and write vectors fail safe.
		{`system call`, `awk 'BEGIN{system("touch /tmp/evil")}'`, false},
		{`output redirect`, `awk 'BEGIN{print "x" > "/tmp/evil"}'`, false},
		{"append redirect", `awk '{print $1 >> "f"}'`, false},
		{`pipe to command`, `awk '{print $1 | "sort"}'`, false},
		{`command fed getline`, `awk '"cmd" | getline x' f`, false},
		{"coprocess", `awk '{c="sort" |& getline l}' f`, false},

		// regex contents that hide an escape or a paren must not mask the vector.
		{`regex hidden system`, `awk '/\"/ {system("x")}' f`, false},
		{"regex hidden redirect", `awk '/\(/ {print $1 > "/tmp/evil"}' f`, false},

		// backslash-newline continuations splice before tokens form: the physical
		// newline must not reset the print tracker or split a keyword
		{"continuation redirect", `awk 'BEGIN{print \
> "/tmp/evil"}'`, false},
		{"continuation pipe", `printf 'x\n' | awk '{print $1 \
| "sort"}'`, false},
		{"continuation crlf redirect", "awk 'BEGIN{print \\\r\n> \"/tmp/evil\"}'", false},
		{"continuation split keyword", `awk 'BEGIN{ sys\
tem("x")}'`, false},
		{"continuation in string", `awk 'BEGIN{print "a \
> b"}'`, true},

		// unquoted continuations join in tokenizeRaw before awk sees the script,
		// so the pipe stays attached to its print
		{"unquoted continuation pipe", "awk BEGIN\\{print\\\n\\|\"id\"}", false},

		// awk comments end at the newline and single quotes are not string
		// delimiters, so an apostrophe or a quote inside one hides nothing
		{"comment apostrophe hides nothing", "awk \"# don't\nBEGIN{system(\\\"id\\\")}\" /dev/null", false},
		{"comment quote hides nothing", "awk '# \" x\nBEGIN{system(\"id\")}'", false},
		{"comment backslash no extend", "awk '# c \\\nBEGIN{system(\"id\")}'", false},
		{"dquote continuation joins", "awk \"BEGIN{print \\\n\\\"hi\\\"}\" f", true},

		// unverifiable flag forms and missing scripts fail safe.
		{"script file", "awk -f prog.awk f", false},
		{"dangling expression", "awk -e", false},
		{"bare awk", "awk", false},
		{"profile writes", `awk --profile=p 'prog'`, false},
		{`load directive`, `awk '@load "x"'`, false},
		{"indirect call", `awk '{s="system"; @s("x")}'`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, awkReadSafe(c.in))
		})
	}
}

func TestAwkScriptReadSafe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain print", "{print $1}", true},
		{"pattern comparison", "$2 > 100", true},
		{"paren comparison", "{print ($2 > 3)}", true},
		{`string literal`, `BEGIN{print "a | b"}`, true},
		{"system call", `BEGIN{system("x")}`, false},
		{"pipe after print", `{print $1 | "sort"}`, false},
		{"coprocess getline", `{c="x" |& getline l}`, false},
		{`load directive`, `@include "f.awk"`, false},

		// spliced continuations must read as one logical statement or keyword
		{"spliced redirect", `{print \
> "f"}`, false},
		{"spliced keyword", `BEGIN{ sys\
tem("x")}`, false},
		{"spliced getline", `BEGIN{ "x" \
| getline v}`, false},
		{"crlf spliced redirect", "{print \\\r\n> \"f\"}", false},
		{"spliced regex pattern", `$1 ~ /a\
b/`, true},
		{"spliced string literal", `BEGIN{print "a\
b"}`, true},
		// mawk splices an escaped backslash's continuation too, keeping the print
		// and its redirect attached, so the splice reading is the one analysed
		{"escaped backslash redirect", "{print $1 \\\\\n> \"f\"}", false},

		// comments end at the newline, single quotes never open a string
		{"comment then system", "# don't\nBEGIN{system(\"x\")}", false},
		{"comment trailing backslash", "# c \\\nBEGIN{system(\"x\")}", false},
		{"apostrophe in string", `BEGIN{print "don't"}`, true},

		// malformed programs refuse instead of guessing
		{"unterminated string", `BEGIN{print "x`, false},
		{"string spans lines", "BEGIN{print \"a\nb\"}", false},
		{"unterminated regex", `$1 ~ /a`, false},
		{"newline in regex", "$1 ~ /a\nb/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, awkScriptReadSafe(c.in))
		})
	}
}
