package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReadOnlyCommandsContents(t *testing.T) {
	t.Parallel()

	want := []string{
		"awk", "ls", "find", "grep", "rg", "diff", "which", "ps", "jq",
		"cat", "echo", "head", "tail", "wc", "file", "cd", "pwd", "sort", "du",
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

func TestWriteCommandsContents(t *testing.T) {
	t.Parallel()

	want := []string{
		"rm", "rmdir", "mv", "cp", "touch", "mkdir", "chmod", "chown",
		"chgrp", "ln", "link", "rename", "dd", "truncate", "tee", "shred",
		"unlink", "mkfifo", "mknod", "install", "mktemp", "split", "patch",
		"setfacl", "setfattr", "chattr",
	}
	assert.Len(t, writeCommands, len(want))
	for _, cmd := range want {
		_, ok := writeCommands[cmd]
		assert.True(t, ok, "%q missing", cmd)
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
		// -fprint and -fprintf are distinct patterns; neither matches the other
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
	for _, re := range findUnsafeFlags {
		if re.MatchString(seg) {
			return true
		}
	}
	return false
}
