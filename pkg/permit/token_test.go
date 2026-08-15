package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTokenizeRaw(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"simple", "ls -la foo", []string{"ls", "-la", "foo"}},
		// adjacent quoted/unquoted pieces join into one token
		{`joined`, `-e's/a/b/'`, []string{"-es/a/b/"}},
		{`double joined`, `sed "s/x/y/" f`, []string{"sed", "s/x/y/", "f"}},
		// backslash escape outside quotes yields the escaped char
		{`backslash`, `a\ b c`, []string{"a b", "c"}},
		{`empty single quote`, `echo '' x`, []string{"echo", "", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, tokenizeRaw(c.in))
		})
	}
}

func TestTokenizeRawDoubleQuoteEscapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		// inside double quotes only \" \\ \$ \` are escapes; others stay literal
		{`escaped dollar`, `echo "a\$b"`, []string{"echo", "a$b"}},
		// \. and \/ before non-escape chars survive verbatim so sed regexes hold
		{`literal backslash kept`, `sed "s/\./\//g"`,
			[]string{"sed", `s/\./\//g`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, tokenizeRaw(c.in))
		})
	}
}

func TestStripPath(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"ls", "ls"},
		{"/usr/bin/ls", "ls"},
		// path strip wins over .sh trim: a slash means the suffix survives
		{"./foo.sh", "foo.sh"},
		{"/x/y/z.sh", "z.sh"},
		// no slash, so only the .sh suffix is trimmed
		{"myscript.sh", "myscript"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, stripPath(c.in), "%q", c.in)
	}
}

func TestUnwrapLaunchers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nohup recurses", []string{"nohup", "ls"}, []string{"ls"}},
		{"timeout basic", []string{"timeout", "30", "ls"}, []string{"ls"}},
		{"timeout value opt", []string{"timeout", "-k", "5", "30", "rm", "-rf", "/"},
			[]string{"rm", "-rf", "/"}},
		{"timeout attached", []string{"timeout", "--signal=TERM", "30s", "git", "status"},
			[]string{"git", "status"}},
		// sudo must not unwrap — it changes privilege
		{"sudo stays", []string{"sudo", "ls"}, []string{"sudo", "ls"}},
		{"env stays", []string{"env", "FOO=1", "cmd"}, []string{"env", "FOO=1", "cmd"}},
		// confused timeout parse returns original tokens
		{"timeout no duration", []string{"timeout", "-k", "5", "ls"},
			[]string{"timeout", "-k", "5", "ls"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, unwrapLaunchers(c.in))
		})
	}
}

func TestSegmentTokens(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"git", "status"}, segmentTokens("timeout 30 git status"))
	// nohup is a launcher and unwraps away entirely
	assert.Equal(t, []string{"ls"}, segmentTokens("nohup ls"))
}
