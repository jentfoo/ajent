package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineContinuation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want int
	}{
		{"lf", "a\\\nb", 2},
		{"crlf", "a\\\r\nb", 3},
		{"escaped backslash", "a\\\\\nb", 0},
		{"ordinary escape", "a\\nb", 0},
		{"trailing backslash", "a\\", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, lineContinuation(c.in, 1))
		})
	}
}

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
		// backslash survives inside single quotes so regexes hold
		{`quoted regex survives`, `grep 'a\.b' f`, []string{"grep", "a\\.b", "f"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, tokenizeRaw(c.in))
		})
	}
}

func TestTokenizeRawContinuations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		// bash removes a backslash-newline before tokens form, argv arriving joined
		{"unquoted joins", "rg --\\\npre=id f", []string{"rg", "--pre=id", "f"}},
		{"crlf joins", "rg --\\\r\npre f", []string{"rg", "--pre", "f"}},
		{"double quoted joins", "awk \"BEGIN{print \\\n\\\"hi\\\"}\"", []string{"awk", `BEGIN{print "hi"}`}},
		// single quotes keep the pair literal, no continuation inside them
		{"single quoted keeps", "sed 'a\\\nb' f", []string{"sed", "a\\\nb", "f"}},
		// an escaped backslash then a newline is no continuation: both survive
		{"escaped backslash keeps", "a\\\\\nb", []string{"a\\\nb"}},
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
		// inside double quotes only \" \\ \$ \` are escapes, others staying literal
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
		// sudo must not unwrap, it changing privilege
		{"sudo stays", []string{"sudo", "ls"}, []string{"sudo", "ls"}},
		{"env stays", []string{"env", "FOO=1", "cmd"}, []string{"env", "FOO=1", "cmd"}},
		// xargs/stdbuf are not launchers and must never unwrap
		{"xargs preserved", []string{"find", ".", "-print0", "|", "xargs", "rm"},
			[]string{"find", ".", "-print0", "|", "xargs", "rm"}},
		// confused timeout parse returns original tokens
		{"timeout no duration", []string{"timeout", "-k", "5", "ls"},
			[]string{"timeout", "-k", "5", "ls"}},
		// only bare time and -p unwrap, GNU's own flags failing safe to the head
		{"time basic", []string{"time", "go", "test", "./..."}, []string{"go", "test", "./..."}},
		{"time posix flag", []string{"time", "-p", "ls"}, []string{"ls"}},
		{"time gnu flag stays", []string{"time", "-f", "%e", "ls"},
			[]string{"time", "-f", "%e", "ls"}},
		{"time output opt stays", []string{"time", "-o", "log", "ls"},
			[]string{"time", "-o", "log", "ls"}},
		{"time alone stays", []string{"time"}, []string{"time"}},
		// nice unwraps through its adjustment grammar, negatives consuming their value
		{"nice bare", []string{"nice", "make", "test"}, []string{"make", "test"}},
		{"nice separate n", []string{"nice", "-n", "5", "go", "test"}, []string{"go", "test"}},
		{"nice negative n", []string{"nice", "-n", "-15", "ls"}, []string{"ls"}},
		{"nice signed long separate", []string{"nice", "--adjustment", "+5", "ls"}, []string{"ls"}},
		{"nice long attached", []string{"nice", "--adjustment=10", "rm", "x"}, []string{"rm", "x"}},
		{"nice long negative", []string{"nice", "--adjustment=-10", "rm", "x"}, []string{"rm", "x"}},
		// a lone dashed token that is no adjustment fails safe to the prompt path
		{"nice unknown flag stays", []string{"nice", "-p", "ls"}, []string{"nice", "-p", "ls"}},
		{"nice invalid value stays", []string{"nice", "-n", "abc", "ls"},
			[]string{"nice", "-n", "abc", "ls"}},
		// a missing command leaves the launcher heading
		{"nice alone stays", []string{"nice"}, []string{"nice"}},
		{"nice value only stays", []string{"nice", "-n", "5"}, []string{"nice", "-n", "5"}},
		// launchers chain, the innermost command surviving
		{"chained launchers", []string{"nohup", "timeout", "30", "time", "git", "status"},
			[]string{"git", "status"}},
		{"nice wraps timeout", []string{"nice", "-n", "5", "timeout", "10", "ls"}, []string{"ls"}},
		// a path-qualified launcher could be any binary, so it never unwraps
		{"absolute launcher stays", []string{"/usr/bin/nohup", "git", "status"},
			[]string{"/usr/bin/nohup", "git", "status"}},
		{"relative launcher stays", []string{"./timeout", "5", "ls"},
			[]string{"./timeout", "5", "ls"}},
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
	// time and nice join the launchers, the wrapped command surviving
	assert.Equal(t, []string{"go", "test", "./..."}, segmentTokens("time go test ./..."))
	assert.Equal(t, []string{"git", "status"}, segmentTokens("nice -n 5 git status"))
	// a path-qualified launcher stays, headOf refusing the head outright
	assert.Equal(t, []string{"/usr/bin/nohup", "ls"}, segmentTokens("/usr/bin/nohup ls"))
}

func TestSkipKeywords(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"none", []string{"rm", "x"}, []string{"rm", "x"}},
		{"single", []string{"do", "git", "push"}, []string{"git", "push"}},
		{"stacked", []string{"{", "rm", "x"}, []string{"rm", "x"}},
		{"all_keywords", []string{"if", "then"}, []string{}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, skipKeywords(c.in))
		})
	}
}

func TestSkipQuoted(t *testing.T) {
	t.Parallel()

	// returns the index past the closing quote, unterminated running to the end
	assert.Equal(t, 3, skipQuoted(`'a' x`, 0))
	assert.Equal(t, 8, skipQuoted(`x "a\"b" y`, 2))
	// backslash escapes only inside double quotes, single quotes literal
	assert.Equal(t, 3, skipQuoted(`'\''`, 0))
	assert.Equal(t, 3, skipQuoted("'ab", 0))
}

func TestHeadOfPathQualified(t *testing.T) {
	t.Parallel()

	// a path-qualified head can point at any binary, so it names nothing: never
	// auto-allows under the basename's verdict and never matches a grant
	for _, seg := range []string{
		"./bin/cat x",
		"../tooling/jq f",
		"/tmp/x/git status",
		`'./cat' x`,              // a quoted spelling keeps the path in the token
		`"git" log`,              // a double quote overlaps a quoted region: no faithful word
		"/usr/bin/nohup git log", // launcher not unwrapped, head refused outright
		"./time ls",              // a path-qualified time could be any binary
	} {
		h, ok := headOf(seg)
		assert.False(t, ok, seg)
		assert.Empty(t, h)
	}

	// a script-suffixed head keeps its literal name, never renamed to the bare word
	h, ok := headOf("deploy.sh --dry-run")
	require.True(t, ok)
	assert.Equal(t, "deploy.sh", h)
}

func TestHeadOfKeywords(t *testing.T) {
	t.Parallel()

	// control keywords head shell syntax, never a nameable command
	for _, seg := range []string{"if", "then rm y", "for", "do head -20 x", "{", "! true"} {
		h, ok := headOf(seg)
		assert.False(t, ok, seg)
		assert.Empty(t, h)
	}

	// a launcher chain unwraps to what actually runs, so a timed write names rm
	h, ok := headOf("timeout 5 time rm x")
	require.True(t, ok)
	assert.Equal(t, "rm", h)
}
