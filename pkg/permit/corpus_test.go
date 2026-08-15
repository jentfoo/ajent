package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
		{"backtick in double unsafe", "echo \\\"\\\\`pwd\\\\`\\\"", false, true},
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
			assert.Equal(t, c.split, s.HasSplitOp, "split")
			assert.Equal(t, c.unsafe, s.HasUnsafeOp, "unsafe")
			if !c.split && !c.unsafe {
				// a clean command stays one segment
				assert.Len(t, s.Segments, 1)
			}
		})
	}
}

func TestTokenizerCorpus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{`sed quoted flag`, `sed "-i" f.txt`, []string{"sed", "-i", "f.txt"}},
		{`sed joined script`, `sed -e's/a/b/' f.txt`, []string{"sed", "-es/a/b/", "f.txt"}},
		{`quoted regex survives`, `grep 'a\.b' f`, []string{"grep", "a\\.b", "f"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, tokenizeRaw(c.in))
		})
	}
}

func TestLauncherCorpus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{`timeout kill after`, []string{"timeout", "-k", "5", "30s", "rm", "-rf", "/"},
			[]string{"rm", "-rf", "/"}},
		{`nohup read only`, []string{"nohup", "ls"}, []string{"ls"}},
		// sudo/env/xargs/nice/stdbuf must never unwrap
		{"sudo preserved", []string{"sudo", "cat", "f"}, []string{"sudo", "cat", "f"}},
		{`xargs preserved`, []string{"find", ".", "-print0", "|", "xargs", "rm"},
			[]string{"find", ".", "-print0", "|", "xargs", "rm"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, unwrapLaunchers(c.in))
		})
	}
}

func TestFindUnsafeFlagCorpus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"find . -exec echo {} ;", true},
		{"find . -delete", true},
		{"find . -fprint0 out", true},
		{"find . -name foo", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, hasFindUnsafeFlag(c.in), "%q", c.in)
	}
}

// TestScannerNeverPanics guards against malformed input crashing the walk.
func TestScannerNeverPanics(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`"`, `'`, `\`, `$(`, `<(`, "2>&1", "2>&12", `a > b \`,
		`""''\\`, "echo \"\\\\", `<<<`, `;;;&&&|||`, `</dev/null`,
	}
	for _, in := range inputs {
		s := scanCommand(in)
		assert.Len(t, s.Raw, len(s.Segments))
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
