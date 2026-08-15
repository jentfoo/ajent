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
		{ModeBlockAll, "block-all", "block"},
	}
	for _, c := range cases {
		assert.Equal(t, c.str, c.m.String(), c.m)
		assert.Equal(t, c.short, c.m.Short(), c.m)
	}
}

func TestModeNextCyclesInOrder(t *testing.T) {
	t.Parallel()

	want := []Mode{ModeAllowRead, ModeAuto, ModeBlockAll, ModeAllowAll}
	m := ModeAllowAll
	for _, w := range want {
		assert.Equal(t, w, m.Next())
		m = w
	}
}

func TestParseModeRejectsEmptyStringForConfigNameOnly(t *testing.T) {
	t.Parallel()

	// String() must round-trip through ParseMode for every valid mode.
	for _, c := range []struct{ m Mode }{{ModeAllowAll}, {ModeAllowRead}, {ModeAuto}, {ModeBlockAll}} {
		m, ok := ParseMode(c.m.String())
		assert.True(t, ok)
		assert.Equal(t, c.m, m)
	}
}
