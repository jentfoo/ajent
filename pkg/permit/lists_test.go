package permit

import (
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReadOnlyCommandsContents(t *testing.T) {
	t.Parallel()

	want := []string{
		"ls", "find", "grep", "diff", "which", "ps", "jq",
		"cat", "echo", "head", "tail", "wc", "cd", "pwd", "du",
		"od",
	}
	assert.Len(t, readOnlyCommands, len(want))
	for _, cmd := range want {
		_, ok := readOnlyCommands[cmd]
		assert.True(t, ok, "%q missing", cmd)
	}
}

func TestNetworkNeverReadOnly(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{"curl", "wget", "nc"} {
		_, ok := readOnlyCommands[cmd]
		assert.False(t, ok, "%q must not be auto-allowed", cmd)
	}
}

// bare heads with a write form stay off the list, gated by their own checkers
func TestWriteFormHeadsNeverBare(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{"sed", "git", "awk", "rg", "sort", "date", "file"} {
		_, ok := readOnlyCommands[cmd]
		assert.False(t, ok, "%q must not be bare-allowed", cmd)
	}
}

func TestFindUnsafeFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"find . -exec rm {} ;", true},
		{"find . -delete", true},
		// -fprint and -fprintf are distinct patterns, neither matching the other
		{"find . -fprint out.txt", true},
		{"find . -fprintf f %p\\n", true},
		{"find . -fls log", true},
		{"find . -name '*.go'", false},
		{"grep -exec foo", true}, // matched on any collapsed segment, not just find
	}
	for _, c := range cases {
		assert.Equal(t, c.want, hasFindUnsafeFlag(c.in), "%q", c.in)
	}
}

// hasFindUnsafeFlag reports whether any unsafe flag matches a segment.
func hasFindUnsafeFlag(seg string) bool {
	return slices.ContainsFunc(findUnsafeFlags, func(re *regexp.Regexp) bool { return re.MatchString(seg) })
}
