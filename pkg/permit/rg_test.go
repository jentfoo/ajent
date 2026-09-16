package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRgReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"plain pattern", []string{"rg", "pattern"}, true},
		{`common flags`, []string{"rg", "-l", "--json", "pattern"}, true},
		{`pre glob inert`, []string{"rg", "--pre-glob", `*.txt`, "pattern"}, true},
		// exec vectors fail safe.
		{`--pre command`, []string{"rg", "--pre", "cmd", "pattern"}, false},
		{`--pre attached`, []string{"rg", "--pre=cmd", "pattern"}, false},
		{`hostname bin`, []string{"rg", "--hostname-bin", "x", "pattern"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, rgReadOnly(c.in))
		})
	}
}
