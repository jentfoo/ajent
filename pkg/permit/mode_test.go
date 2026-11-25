package permit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModeParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"", ModeAllowRead, true}, // empty means default
		{"allow-read", ModeAllowRead, true},
		{"allow-all", ModeAllowAll, true},
		{"auto", ModeAuto, true},
		{"auto+mcp", ModeAuto, true}, // legacy alias for auto
		{"auto+write", ModeAutoWrite, true},
		{"block-all", ModeBlockAll, true},
		{"bogus", 0, false},
	}
	for _, c := range cases {
		m, ok := ParseMode(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		if ok {
			assert.Equal(t, c.want, m, c.in)
		}
	}
}

// String() must pin the exact user-visible spelling and round-trip through
// ParseMode for every valid mode
func TestModeStringRoundTrip(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		m     Mode
		str   string
		short string
	}{
		{ModeAllowAll, "allow-all", "all"},
		{ModeAllowRead, "allow-read", "read"},
		{ModeAuto, "auto", "auto"},
		{ModeAutoWrite, "auto+write", "auto+w"},
		{ModeBlockAll, "block-all", "block"},
	} {
		assert.Equal(t, c.str, c.m.String(), c.m)
		assert.Equal(t, c.short, c.m.Short(), c.m)

		got, ok := ParseMode(c.m.String())
		assert.True(t, ok)
		assert.Equal(t, c.m, got)
	}
}

func TestModeCycles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		step func(Mode) Mode // the cycle direction under test
		want []Mode          // every step from ModeAllowRead back around to itself
	}{
		{"next", Mode.Next, []Mode{ModeAuto, ModeAutoWrite, ModeAllowAll, ModeBlockAll, ModeAllowRead}},
		{"prev", Mode.Prev, []Mode{ModeBlockAll, ModeAllowAll, ModeAutoWrite, ModeAuto, ModeAllowRead}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ModeAllowRead
			for _, w := range c.want {
				assert.Equal(t, w, c.step(m))
				m = w
			}
		})
	}

	t.Run("round_trip", func(t *testing.T) {
		// Prev is the exact inverse of Next over the whole cycle
		for _, m := range []Mode{ModeAllowAll, ModeAllowRead, ModeAuto, ModeAutoWrite, ModeBlockAll} {
			assert.Equal(t, m, m.Next().Prev())
			assert.Equal(t, m, m.Prev().Next())
		}
	})
}
