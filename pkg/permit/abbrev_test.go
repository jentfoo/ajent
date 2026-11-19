package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAbbrevLong(t *testing.T) {
	t.Parallel()

	flags := []string{"--output", "--ext-diff"}
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"exact", "--output", true},
		{"eq value", "--output=out.patch", true},
		{"proper prefix", "--ext-dif", true},
		{"minimal prefix", "--o", true},
		{"prefix eq value", "--outp=/tmp/x", true},
		// anything not resolving to a flag stays unmatched
		{"bare terminator", "--", false},
		{"short flag", "-o", false},
		{"operand", "file", false},
		{"longer than flag", "--outputs", false},
		{"not a prefix", "--filterx", false},
		{"negated form", "--no-ext-diff", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, abbrevLong(c.in, flags...))
		})
	}
}
