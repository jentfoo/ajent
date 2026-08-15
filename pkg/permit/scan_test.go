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
		// $() inside double quotes expands and executes — unsafe but not a split
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

	for _, in := range []string{"a && b", "a || b", "a | b", "a;b", "a & b", "a\nb"} {
		s := scanCommand(in)
		assert.True(t, s.HasSplitOp, "%q should split", in)
	}
}

func TestScanUnterminatedQuoteIsUnsafe(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`echo "unclosed`, `echo 'oops`} {
		s := scanCommand(in)
		assert.True(t, s.HasUnsafeOp, "%q", in)
	}
}

func TestMatchNullRedirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		i    int
		want int
	}{
		{"> /dev/null", 0, len("> /dev/null")},
		{"cmd >/dev/null foo", 4, len(">/dev/null")},
		// at the '1' the digit-fd guard fires (prev 'e'), so no match there
		{"cat file1>/dev/null", 8, 0},
		{"cat file1>/dev/null", 9, len(">/dev/null")}, // plain > at index 9 matches
		{">/dev/nullfoo", 0, 0},                       // real file named /dev/nullfoo
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, matchNullRedirect(c.in, c.i))
		})
	}
}

func TestCompoundDetectsPipesAndSubstitution(t *testing.T) {
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
