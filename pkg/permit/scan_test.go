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
		// $() inside double quotes expands and executes, unsafe but not a split
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
		{`plain redirect`, "cat f > out", false, false},
		{`dev null discards`, "ls > /dev/null", false, false},
		{"amp dev null not split", "cmd &>/dev/null", false, false},
		{"stderr merge", "cmd 2>&1", false, false},
		// bare 2>&1 discards, the & in 2>&12 reading as a background operator
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
		{`quoted find delete`, `find . "-delete"`, true},
		{"nested interpreter", `sh -c "ls"`, false}, // compound is about splits, not nameability
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
		{"find . -delete", false},   // unsafe find action
		{`find . "-delete"`, false}, // quoting cannot hide the action
		{`find . "-exec" rm {} \;`, false},
		{`find "-fprintf" x f`, false},
		{`nohup find . "-delete"`, false},
		{"ls FOO=1", false},           // argument-position assignment is still shell state
		{`sh -c "git status"`, false}, // nested interpreter: unnameable
		{`bash -c 'ls'`, false},
		{`eval ls`, false},
		{"FOO=1 ls", false},          // leading assignment already failed closed
		{"echo hi > out.txt", false}, // unsafe redirect
		{"curl -s https://x", false}, // not on the allowlist
	}
	for _, c := range cases {
		assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)), c.in)
	}
}

func TestExpansionUnresolved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// $ and backtick stay live outside single quotes
		{"dollar arg", "cat $F", true},
		{"dollar in dquote", `awk "BEGIN{print \"$X\"}"`, true},
		{"backtick arg", "cat `pwd`", true},
		{"dollar escaped", `cat \$F`, false},
		{"dollar single quoted", `awk '{print $1}'`, false},
		// brace patterns rewrite into several words after analysis
		{"brace comma", "file {--compile,-m} ./magic", true},
		{"brace range", "date --{se,t}=x", true},
		{"brace literal keeps", "date -d 'a{2}' f", false},
		// ansi-c quoting keeps its escaping outside single quotes
		{"ansi-c quote", "file $'-C' -m ./magic", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, expansionUnresolved(c.in), c.in)
		})
	}
}

func TestExpansionBlocksReadOnlyVerdict(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"file {--compile,-m} ./magic",
		"file $'-C' -m ./magic",
		"date --{se,t}=2025-01-01",
		"rg --\\\npre=id f",
		"sort --\\\noutput=/tmp/evil f",
		"date --\\\nset=2030-01-01",
	} {
		assert.False(t, allSegmentsReadOnly(scanCommand(in)), in)
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
		{"append", "cmd >> log", false, false},
		{"stderr merge ok", "cmd 2>&1", false, false},
		{"fd twelve real target", "cmd 2>&12", true, true},
		{"amp dev null inert", "ls &>/dev/null", false, false},
		{"word digit not eaten", "cat file1> /dev/null", false, false},
		{`dev null foo is a file`, `echo hi >/dev/nullfoo`, false, false},
		{`backtick unsafe`, "cmd `id`", false, true},
		// input redirect: an ordinary file target stays auto-allowable and never
		// records as a write, the network pseudo-device flagging unsafe instead.
		// Output and <> forms record and prompt through the write paths.
		{"input plain file", "cat < Makefile", false, false},
		{"input network tcp", "cat < /dev/tcp/host/4444", false, true},
		{"input network udp", "cat < /dev/udp/host/53", false, true},
		{"input network pipeline", "head -c 1M < /dev/tcp/host/port | jq .", true, true},
		{"input fd prefix network", "cat 0</dev/tcp/host/port", false, true},
		{"read write form network", "cat <> /dev/tcp/host/port", false, false},
		{"output network", "cmd > /dev/tcp/host/port", false, false},
		{"append network", "cmd >> /dev/tcp/host/port", false, false},
		{"input missing word", "cat <", false, true},
		{"input operator follows", "cat < ; ls", true, true},
		{"input procsub still unsafe", "cat <(id)", false, true},
		{"here string not input redirect", "grep -f <<< x", false, false},
		{"heredoc marker no phantom", "cat <<EOF", false, true},
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

func TestScanHeredocs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		segments []string
		unsafe   bool
	}{
		// the body is data: python inside a quoted delimiter is never a command
		{"quoted body is data", "python - <<'EOF'\nimport importlib.metadata as md\nprint(\"mcp\", md.version(\"mcp\"))\nEOF\n", []string{`python - <<""`}, false},
		{"unquoted body clean", "python - <<EOF\nprint(1)\nEOF", []string{"python - <<EOF"}, false},
		// an unquoted delimiter expands, so substitution in the body is unsafe
		{"unquoted body subst", "python - <<EOF\nprint($(rm x))\nEOF", []string{"python - <<EOF"}, true},
		{"unquoted body backtick", "python - <<EOF\nprint(`id`)\nEOF", []string{"python - <<EOF"}, true},
		// a quoted delimiter keeps the body literal, substitution inert
		{"quoted body subst inert", "python - <<'EOF'\nprint($(rm x))\nEOF", []string{`python - <<""`}, false},
		{"quoted delimiter spaces", "cat <<\"E O F\"\nx\nE O F", []string{`cat <<""`}, false},
		// body then later commands: only shell lines become segments
		{"body precedes later commands", "python - <<'EOF'\nimport md\nEOF\necho done\ncd /tmp && python - <<'EOF'\nprint(1)\nEOF\n", []string{`python - <<""`, "echo done", "cd /tmp", `python - <<""`}, false},
		{"tab stripped terminator", "cmd <<-END\n\tbody\n\tEND\necho done", []string{"cmd <<-END", "echo done"}, false},
		{"two markers one line", "cat <<A <<B\na1\nA\nb1\nB", []string{"cat <<A <<B"}, false},
		{"terminator must be whole line", "cat <<E\nEOFX\nE", []string{"cat <<E"}, false},
		{"missing terminator unsafe", "python - <<'EOF'\nimport md", []string{`python - <<""`}, true},
		{"dangling marker unsafe", "if :; then cat <<E; fi", []string{"if :", "then cat <<E", "fi"}, true},
		// <<< is a here-string, its word scanned normally
		{"here string not heredoc", "grep -f <<< x", []string{"grep -f <<< x"}, false},
		// << inside arithmetic is a shift, never a marker (the $( itself is unsafe)
		{"shift inside arithmetic", "echo $((1<<2)) && rm -rf /", []string{"echo $((1<<2))", "rm -rf /"}, true},
		// a delimiter the marker parser rejects leaves the second < reading as one
		// input redirect: its target word drops, the body lines stay segments, and
		// nothing may read a backwards index
		{"newline crossing delimiter quote", "cat <<\\'E\nbody\nE'\\'tail", []string{"cat <", "body", `E""tail`}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.segments, s.Segments)
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
			assert.Len(t, s.Raw, len(s.Segments))
		})
	}
}

func TestScanHeredocBodies(t *testing.T) {
	t.Parallel()

	// the body text rides to the segment of the command reading it, so the deny
	// path can scan it without a second parser
	cases := []struct {
		name   string
		in     string
		bodies []string
		unsafe bool
	}{
		{"single_reader", "sh <<EOF\nrm -rf /\nEOF", []string{"rm -rf /"}, false},
		{"reader_in_compound", "echo hi; sh <<E\nrm x\nE", []string{"", "rm x"}, false},
		{"commands_after_body", "cat <<E\nhello\nE\necho done", []string{"hello"}, false},
		{"two_docs_one_reader", "sh <<A <<B\na\nA\nb\nB", []string{"a\nb"}, false},
		{"tab_stripped_lines", "sh <<-E\n\trm x\n\tE", []string{"rm x"}, false},
		{"quoted_delim_body", "sh <<'E'\nrm -rf /\nE", []string{"rm -rf /"}, false},
		{"empty_body_skipped", "cat <<E\nE", nil, false},
		{"unterminated_kept_unsafe", "sh <<E\nrm x", []string{"rm x"}, true},
		{"no_heredoc_none", "ls -la", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.bodies, s.HeredocBodies)
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
			assert.LessOrEqual(t, len(s.HeredocBodies), len(s.Segments))
		})
	}
}

func TestScanHeredocQuotedAlignment(t *testing.T) {
	t.Parallel()

	// quoted tracks per segment whether every body feeding it had a quoted
	// delimiter, any unquoted one leaving expansion live in the joined text
	cases := []struct {
		name   string
		in     string
		quoted []bool
	}{
		{"unquoted", "cat <<E\nx\nE", []bool{false}},
		{"single_quoted", "cat <<'E'\nx\nE", []bool{true}},
		{"double_quoted", "cat <<\"E\"\nx\nE", []bool{true}},
		{"mixed_docs_one_reader", "cat <<A <<'B'\na\nA\nb\nB", []bool{false}},
		{"no_heredoc", "ls -la", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.quoted, s.HeredocQuoted)
			assert.Len(t, s.HeredocQuoted, len(s.HeredocBodies))
		})
	}
}

func TestScanRedirectTargets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		in         string
		redirects  []Redirect
		unresolved bool
		unsafe     bool
	}{
		{"plain", "go test ./... > /tmp/t.log", []Redirect{{Target: "/tmp/t.log"}}, false, false},
		{"append", "cmd >> log", []Redirect{{Target: "log"}}, false, false},
		{"stderr fd", "cmd 2> err.log", []Redirect{{Target: "err.log"}}, false, false},
		{"target dropped from segment", "ls > out", []Redirect{{Target: "out"}}, false, false},
		{"stderr merge none", "cmd 2>&1", nil, false, false},
		{"dev null none", "ls > /dev/null", nil, false, false},
		{"two targets", "cmd > a 2> b", []Redirect{{Target: "a"}, {Target: "b"}}, false, false},
		{"quoted", `cmd > "my file"`, []Redirect{{Target: "my file", Raw: `"my file"`}}, true, false},
		{"escaped space", `cmd > my\ file`, []Redirect{{Target: "my file"}}, false, false},
		{"variable", "cmd > $OUT", []Redirect{{Target: "$OUT"}}, true, false},
		{"home", "cmd > ~/x", []Redirect{{Target: "~/x"}}, true, false},
		{"glob", "cmd > f*", []Redirect{{Target: "f*"}}, true, false},
		{"brace", "cmd > {a,b}", []Redirect{{Target: "{a,b}"}}, true, false},
		{"missing word", "cmd >", nil, false, true},
		{"operator follows", "cmd > ; ls", nil, false, true},
		{"unterminated quote", `cmd > "oops`, nil, false, true},
		// procsub/subshell never parse as a target word: bash would execute it
		{"procsub", "cmd 1>(sh)", nil, false, true},
		{"append procsub", "cmd >>(sh)", nil, false, true},
		{"procsub to cmd", "cmd > >(tr a-z A-Z)", nil, false, true},
		{"paren mid word", "cmd > f(o)", []Redirect{{Target: "f"}}, false, true},
		// a backslash-newline is a line continuation: bash's target word differs
		{"escaped newline", "cmd > \\" + "\n/etc/passwd", nil, false, true},
		// input targets never record: only the network form matters, flagged as
		// unsafe by the scan itself
		{"input plain file none", "cat < Makefile", nil, false, false},
		{"input network", "cat < /dev/tcp/host/4444", nil, false, true},
		{"input fd prefix network", "cat 0</dev/tcp/h/p", nil, false, true},
		{"output network records", "cmd > /dev/tcp/host/port", []Redirect{{Target: "/dev/tcp/host/port"}}, false, false},
		{"read write form", "cmd <> f", []Redirect{{Target: "f"}}, false, false},
		{"read write network records", "cmd <> /dev/tcp/h/p", []Redirect{{Target: "/dev/tcp/h/p"}}, false, false},
		{"heredoc no phantom target", "cat <<EOF", nil, false, true},
		{"here string no target", "grep -f <<< x", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanCommand(c.in)
			assert.Equal(t, c.redirects, s.Redirects)
			assert.Equal(t, c.unresolved, s.RedirectsUnresolved())
			assert.Equal(t, c.unsafe, s.HasUnsafeOp)
		})
	}
}

func TestHasNetworkRedirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"no redirects", "cat f", false},
		{"output file", "cat f > out", false},
		{"input file", "cat < f", false},
		{"output tcp", "cat f > /dev/tcp/h/p", true},
		{"input tcp", "cat < /dev/tcp/h/p", false}, // input targets never record
		{"input udp", "cat < /dev/udp/h/53", false},
		{"both mixed", "cat < f > /dev/tcp/h/p", true},
		{"read write form", "cat <> /dev/tcp/h/p", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, scanCommand(c.in).HasNetworkRedirect())
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
		// never panic on arbitrary bytes, segments and raw staying index-aligned
		assert.Len(t, s.Raw, len(s.Segments))
	})
}
