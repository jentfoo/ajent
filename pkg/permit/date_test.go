package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDateReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"plain", []string{"date"}, true},
		{"utc flag", []string{"date", "-u"}, true},
		{"format operand", []string{"date", "+%Y-%m-%d"}, true},
		{"iso attached", []string{"date", "-Iseconds"}, true},
		{"rfc long", []string{"date", "--rfc-2822"}, true},
		{"date value separate", []string{"date", "-d", "yesterday"}, true},
		{"date value attached", []string{"date", "-d+2 hours"}, true},
		{"date long eq", []string{"date", "--date=yesterday"}, true},
		{"reference file", []string{"date", "-r", "main.go", "+%s"}, true},
		{"option terminator", []string{"date", "--", "+%s"}, true},
		// -j opts out of setting, so the BSD/macOS conversion stays free
		{"bsd convert", []string{"date", "-j", "-f", "%Y-%m-%d", "2025-01-01", "+%s"}, true},
		{"bsd convert cluster", []string{"date", "-jf", "%Y-%m-%d", "2025-01-01", "+%s"}, true},
		{"display adjust attached", []string{"date", "-v+1d"}, true},
		// clock-set vectors fail safe
		{"set separate", []string{"date", "-s", "2025-01-01"}, false},
		{"set attached", []string{"date", "-s2025-01-01"}, false},
		{"set in cluster", []string{"date", "-us", "2025-01-01"}, false},
		{"long set", []string{"date", "--set", "2025-01-01"}, false},
		{"long set eq", []string{"date", "--set=2025-01-01"}, false},
		// getopt_long abbreviations of --set
		{"set minimal abbrev", []string{"date", "--s", "2025-01-01"}, false},
		{"set abbrev eq", []string{"date", "--se=2025-01-01"}, false},
		// a bare new_date operand sets without -j on BSD/macOS
		{"bsd numeric operand", []string{"date", "031012002025"}, false},
		{"bsd operand with seconds", []string{"date", "-u", "031012002025.30"}, false},
		{"operand after value flag", []string{"date", "-d", "x", "03101200"}, false},
		// -t sets the BSD kernel timezone
		{"tz set separate", []string{"date", "-t", "300"}, false},
		{"tz set attached", []string{"date", "-t300"}, false},
		// options off the tables are unverifiable: unknown spellings and busybox's
		// hardware-clock -S fail safe
		{"busybox hwclock set", []string{"date", "-S", "2025-01-01"}, false},
		{"unknown short", []string{"date", "-q"}, false},
		{"unknown long", []string{"date", "--nope"}, false},
		{"unknown in cluster", []string{"date", "-uq"}, false},
		{"utc abbreviation", []string{"date", "--ut"}, true},
		{"option terminator only", []string{"date", "--"}, true},
		{"bare dash operand", []string{"date", "-"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, dateReadOnly(c.in))
		})
	}
}

func TestDateQuotedFlagSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// quoted flags reach the checker through tokenizeRaw's verbatim raw text
		{"quoted set short", `date "-s" 2025-01-01`, false},
		{"single quoted long set", `date '--set' 2025-01-01`, false},
		{"quoted benign value flag", `date "-d" "+2 hours"`, true},
		{"quoted numeric operand", `date "031012002025"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)))
		})
	}
}
