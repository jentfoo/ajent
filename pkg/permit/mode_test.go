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

func TestModeStrings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		m     Mode
		str   string
		short string
	}{
		{ModeAllowAll, "allow-all", "all"},
		{ModeAllowRead, "allow-read", "read"},
		{ModeAuto, "auto", "auto"},
		{ModeAutoWrite, "auto+write", "auto+w"},
		{ModeBlockAll, "block-all", "block"},
	}
	for _, c := range cases {
		assert.Equal(t, c.str, c.m.String(), c.m)
		assert.Equal(t, c.short, c.m.Short(), c.m)
	}
}

func TestModeNextCyclesInOrder(t *testing.T) {
	t.Parallel()

	want := []Mode{ModeAuto, ModeAutoWrite, ModeAllowAll, ModeBlockAll, ModeAllowRead}
	m := ModeAllowRead
	for _, w := range want {
		assert.Equal(t, w, m.Next())
		m = w
	}
}

func TestModeStringRoundTrip(t *testing.T) {
	t.Parallel()

	// String() must round-trip through ParseMode for every valid mode.
	for _, m := range []Mode{ModeAllowAll, ModeAllowRead, ModeAuto, ModeAutoWrite, ModeBlockAll} {
		got, ok := ParseMode(m.String())
		assert.True(t, ok)
		assert.Equal(t, m, got)
	}
}
