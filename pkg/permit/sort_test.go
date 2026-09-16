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
		// write and exec vectors fail safe
		{"output separate", []string{"sort", "-o", "out", "in"}, false},
		{"output attached", []string{"sort", "-oout", "in"}, false},
		{`cluster output`, []string{"sort", "-ro", "out", "in"}, false},
		{`long output eq`, []string{"sort", "--output=out", "in"}, false},
		{`compress program`, []string{"sort", "--compress-program=gzip", "f"}, false},
		{`temp directory`, []string{"sort", "-T", "/elsewhere", "f"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sortReadOnly(c.in))
		})
	}
}
