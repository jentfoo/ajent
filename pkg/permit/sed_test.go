package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSedWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain", `sed -i 's/a/b/' f.txt`, true},
		{"short cluster", "sed -ni s/a/b/ f", true},
		{"extended short", "sed -Ei s/a/b/ f", true},
		{"backup suffix", "sed -i.bak s/a/b/ f", true},
		{`quoted flag`, `sed "-i" f.txt`, true},
		{`single quoted`, `sed '-ni' f.txt`, true},
		{"long in place", "sed --in-place s/a/b/ f", true},
		{"long with eq", "sed --in-place=s.bak s/a/b/ f", true},
		// attached -e script is not read as -i (lowercase e absent from shorts)
		{`attached script`, `sed -eis/x/y/ f.txt`, false},
		{"read only sed", "sed 's/a/b/' f.txt", false},
		{"no in place flag", "sed -n p f.txt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sedWrite(c.in))
		})
	}
}

func TestSedReadSafeScripts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"substitution", "sed 's/a/b/' f.txt", true},
		{`global sub`, `sed 's/foo/bar/g' f.txt`, true},
		{`case flags`, `sed 's/a/b/Ig' f.txt`, true},
		{`numbered flag`, `sed 's/a/b/2' f.txt`, true},
		{"delete", "sed '/^#/d' f.txt", true},
		{"print", "sed -n '1,5p' f.txt", true},
		{"quit", "sed '10q' f.txt", true},
		{"translit", "sed 'y/a-z/A-Z/' f.txt", true},
		{`negated addr`, `sed '/foo/!d' f.txt`, true},
		{`range with subst`, `sed '1,/end/s/x/y/g' f.txt`, true},
		{"step address", "sed '0~2p' f.txt", true},
		// write / exec forms fail
		{`subst w flag`, `sed 's/a/b/w out.txt' f.txt`, false},
		{`subst e flag`, `sed 's/a/b/e' f.txt`, false},
		{"hold space h ok", "sed -n '/x/h;/y/g' f.txt", true},
		// script from file is unverifiable
		{"script file", "sed -f script.sed f.txt", false},
		{`long file flag`, `sed --file=script.sed f.txt`, false},
		// missing value for -e fails safe
		{"dangling expression", "sed -e", false},
		// positional script plus input files
		{"positional script", "sed s/a/b/ f1 f2", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sedReadSafe(c.in))
		})
	}
}

func TestSedCommandReadSafe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"subst", "s/a/b/", true},
		{"non slash delimiter", "s|a|b|g", true},
		{"range delete", "1,5d", true},
		{"regex address", "/re/d", true},
		{"last line print", "$p", true},
		{"bare negate invalid", "!p", false}, // bare ! with no address is not a valid command
		{"subst write flag", "s/a/b/w f", false},
		{"translit", "y/a-z/A-Z/", true},
		{"hold get read", "g", true}, // hold/get read commands
		{"exchange buffers", "x", true},
		{"quit fine", "q", true},
		{"subst exec flag", "s/a/b/e", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sedCommandReadSafe(c.in))
		})
	}
}

func TestParseSedSubst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// an escaped delimiter inside the pattern must not terminate it
		{"escaped delim in body", `s/foo\/bar/baz/g`, true},
		{"too many parts bad flag", "s/a/b/c/d", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, parseSedSubst(c.in))
		})
	}
}

func TestParseSedTranslit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain translit", "y/a-z/A-Z/", true},
		// a trailing flag is rejected: transliteration takes no flags
		{"trailing flag rejected", "y/a-z/A-Z/g", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, parseSedTranslit(c.in))
		})
	}
}
