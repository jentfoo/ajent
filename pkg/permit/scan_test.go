package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScanCommandSegmentsAndRawAligned(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		segments []string
	}{
		{"simple", "ls -la", []string{"ls -la"}},
		{"operators", "a && b; c | d & e",
			[]string{"a", "b", "c", "d", "e"}},
		// quoted region collapses to "" so pattern matching only sees code
		{`quoted`, `echo "x y"; ls`, []string{`echo ""`, "ls"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Len(t, s.Raw, len(s.Segments))
			assert.Equal(t, c.segments, s.Segments)
		})
	}
}

func TestScanOperatorsInsideQuotesAreInert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		split  bool
		unsafe bool
	}{
		{`semicolon in double`, `echo "a;b"`, false, false},
		{"redirect in single", "echo 'x > y'", false, false},
		// $() inside double quotes expands and executes; unsafe but not a split
		{"subst in double", `echo "$(pwd)"`, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.split, s.HasSplitOp)
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
		})
	}
}

func TestScanRedirects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		split  bool
		unsafe bool
	}{
		{`plain redirect`, "cat f > out", false, true},
		{`dev null discards`, "ls > /dev/null", false, false},
		{"amp dev null not split", "cmd &>/dev/null", false, false},
		{"stderr merge", "cmd 2>&1", false, false},
		// bare 2>&1 discards; the & in 2>&12 reads as a background operator
		{"fd twelve is a real target", "cmd 2>&12", true, true},
		{"word digit not eaten", "cat file1> /dev/null", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.split, s.HasSplitOp)
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
		})
	}
}

func TestScanSplitOps(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, in string }{
		{"double amp", "a && b"},
		{"or op", "a || b"},
		{"pipe", "a | b"},
		{"semicolon", "a;b"},
		{"background", "a & b"},
		{"newline", "a\nb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.True(t, scanCommand(c.in).HasSplitOp)
		})
	}
}

func TestScanUnterminatedQuoteIsUnsafe(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, in string }{
		{"double unclosed", `echo "unclosed`},
		{"single unclosed", `echo 'oops`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.True(t, scanCommand(c.in).HasUnsafeOp)
		})
	}
}

func TestMatchNullRedirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		i    int
		want int
	}{
		{"spaced", "> /dev/null", 0, len("> /dev/null")},
		{"mid command", "cmd >/dev/null foo", 4, len(">/dev/null")},
		// at the '1' the digit-fd guard fires (prev 'e'), so no match there
		{"digit fd guarded", "cat file1>/dev/null", 8, 0},
		{"plain gt after digit", "cat file1>/dev/null", 9, len(">/dev/null")}, // plain > at index 9 matches
		{"nullfoo is a file", ">/dev/nullfoo", 0, 0},                          // real file named /dev/nullfoo
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, matchNullRedirect(c.in, c.i))
		})
	}
}

func TestCompound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "ls", false},
		{"pipeline", "a | b", true},
		{`process substitution`, `grep -f <(echo x)`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, compound(c.in))
		})
	}
}

func TestAllSegmentsReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"ls -la", true},
		{"grep foo f | sort", true}, // pipeline tolerated
		{"git status && git log --oneline", true},
		{`sed -n 's/a/b/p' f`, true},
		{"rm -rf build", false},
		{"find . -delete", false},    // unsafe find action
		{"echo hi > out.txt", false}, // unsafe redirect
		{"curl -s https://x", false}, // not on the allowlist
	}
	for _, c := range cases {
		assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)), c.in)
	}
}

func TestClearWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"ls -la", false},
		{"rm build && ls", true},
		{"grep foo f | sort", false},
		{"sed -i s/a/b/ f", false}, // sed handled by its own analyser, not the write list
	}
	for _, c := range cases {
		assert.Equal(t, c.want, clearWrite(scanCommand(c.in)), c.in)
	}
}

// corpusCase pins the scan-level facts a command must satisfy: whether it splits,
// carries an unsafe op, and how many collapsed segments result.
type corpusCase struct {
	name   string
	in     string
	split  bool
	unsafe bool
}

func TestScannerCorpus(t *testing.T) {
	t.Parallel()

	cases := []corpusCase{
		{"read only ls", "ls -la", false, false},
		{"pipeline split", `grep foo f.txt | sort`, true, false},

		// quoting
		{`semicolon single quoted`, `echo 'a;b'`, false, false},
		{`ampersand double quoted`, `echo "x & y"`, false, false},
		{`pipe inside quotes`, `grep "|" f`, false, false},
		{`subst in double unsafe`, `echo "$(date)"`, false, true},
		{"backtick in double unsafe", "echo \"`pwd`\"", false, true},
		{`unterminated single`, `cat 'oops`, false, true},
		{`escaped space outside quotes`, `ls a\ b`, false, false},

		// redirects
		{"append", "cmd >> log", false, true},
		{"stderr merge ok", "cmd 2>&1", false, false},
		{"fd twelve real target", "cmd 2>&12", true, true},
		{"amp dev null inert", "ls &>/dev/null", false, false},
		{"word digit not eaten", "cat file1> /dev/null", false, false},
		{`dev null foo is a file`, `echo hi >/dev/nullfoo`, false, true},
		{`backtick unsafe`, "cmd `id`", false, true},

		// split operators
		{"double amp", "a && b", true, false},
		{"or op", "a || b", true, false},
		{"semicolon", "a;b", true, false},
		{"background", "sleep 5 &", true, false},
		{"newline", "a\nb", true, false},

		// process substitution
		{`process subst`, `grep -f <(cat x)`, false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.split, s.HasSplitOp)
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
		})
	}
}

func FuzzScan(f *testing.F) {
	f.Add("ls")
	f.Add(`echo "$(pwd)"`)
	f.Add("cmd 2>&1 && ls > /dev/null")
	f.Add(`sed -i 's/a/b/' f`)
	f.Add("cat file1> /dev/null")
	f.Fuzz(func(t *testing.T, in string) {
		s := scanCommand(in)
		// never panic on arbitrary bytes; segments and raw stay index-aligned
		assert.Len(t, s.Raw, len(s.Segments))
	})
}
