package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFileReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"plain file", []string{"file", "bin/ajent"}, true},
		{"brief mime", []string{"file", "-b", "--mime-type", "img.png"}, true},
		{"magic source", []string{"file", "-m", "./magic", "f"}, true},
		{"flag cluster", []string{"file", "-bL", "f"}, true},
		{"lowercase checking", []string{"file", "-c", "f"}, true},
		// magic-database compilation writes, fail safe
		{"compile short", []string{"file", "-C"}, false},
		{"compile cluster", []string{"file", "-bC", "-m", "./magic"}, false},
		{"compile long", []string{"file", "--compile", "-m", "./magic"}, false},
		{"compile abbrev", []string{"file", "--comp", "-m", "./magic"}, false},
		{"compile minimal abbrev", []string{"file", "--c"}, false},
		{"compile eq", []string{"file", "--compile=1"}, false},
		// helper-running and sandbox-dropping spellings fail safe as unknowns
		{"uncompress helper", []string{"file", "-z", "f.gz"}, false},
		{"uncompress noread", []string{"file", "-Z", "f.gz"}, false},
		{"preserve date utimes", []string{"file", "-p", "f"}, false},
		{"no sandbox long", []string{"file", "--no-sandbox", "f"}, false},
		{"preserve long", []string{"file", "--preserve-date", "f"}, false},
		// unknown spellings prompt rather than allow
		{"unknown short", []string{"file", "-q", "f"}, false},
		{"unknown long", []string{"file", "--nope"}, false},
		{"short with value ok", []string{"file", "-m", "./magic"}, true},
		{"option terminator only", []string{"file", "--"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, fileReadOnly(c.in))
		})
	}
}

func TestFileQuotedFlagSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// quoted flags reach the checker through tokenizeRaw's verbatim raw text
		{"quoted compile", `file "-C"`, false},
		{"quoted benign", `file "-b" f`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)))
		})
	}
}
