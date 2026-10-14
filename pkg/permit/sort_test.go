package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"plain file", []string{"sort", "f"}, true},
		{`unique flag`, []string{"sort", "-u", "f"}, true},
		{"benign long flags", []string{"sort", "--stable", "--numeric-sort", "f"}, true},
		{"benign long abbrev", []string{"sort", "--stab", "f"}, true},
		{"benign ambiguous abbrev", []string{"sort", "--m", "f"}, true},
		{"compress minimal abbrev", []string{"sort", "--c", "f"}, false},
		{"option terminator", []string{"sort", "--", "f"}, true},
		// write and exec vectors fail safe
		{"output separate", []string{"sort", "-o", "out", "in"}, false},
		{"output attached", []string{"sort", "-oout", "in"}, false},
		{`cluster output`, []string{"sort", "-ro", "out", "in"}, false},
		{"long output eq", []string{"sort", "--output=out", "in"}, false},
		{"compress program", []string{"sort", "--compress-program=gzip", "f"}, false},
		{"temp directory", []string{"sort", "-T", "/elsewhere", "f"}, false},
		// getopt_long abbreviations of the write/exec flags
		{"output abbrev eq", []string{"sort", "--outp=/tmp/x", "in"}, false},
		{"output minimal abbrev", []string{"sort", "--o", "/tmp/x", "in"}, false},
		{"compress abbrev", []string{"sort", "--comp=gzip", "f"}, false},
		{"temp abbrev separate", []string{"sort", "--temp", "/elsewhere", "f"}, false},
		{"temp minimal abbrev", []string{"sort", "--t", "/elsewhere", "f"}, false},
		{"temp abbrev eq", []string{"sort", "--temp=/elsewhere", "f"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sortReadOnly(c.in))
		})
	}
}

func TestSortQuotedFlagSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// quoted flags reach the checker through tokenizeRaw's verbatim raw text
		{"quoted output abbrev", `sort "--outp=/tmp/x" f`, false},
		{"single quoted minimal", `sort '--o' /tmp/x f`, false},
		{"quoted benign", `sort "--stable" f`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)))
		})
	}
}

func TestSortLongWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"output exact", "--output", true},
		{"output eq", "--output=out", true},
		{"output prefix", "--outp", true},
		{"minimal prefix", "--o", true},
		{"compress exact", "--compress-program", true},
		{"compress prefix", "--comp", true},
		{"temp exact", "--temporary-directory", true},
		{"temp prefix", "--temp", true},
		{"temp eq prefix", "--temp=/d", true},
		// anything not a prefix of the three flags stays unmatched
		{"bare dashdash", "--", false},
		{"bare dash", "-", false},
		{"short flag", "-o", false},
		{"benign flag", "--stable", false},
		{"benign prefix", "--n", false},
		{"compress minimal prefix", "--c", true},
		{"longer than flag", "--output-dir", false},
		{"not a prefix", "--tempdir", false},
		{"operand", "file", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sortLongWrite(c.in))
		})
	}
}
