package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDividerRow(t *testing.T) {
	t.Parallel()

	t.Run("solid_band_with_background", func(t *testing.T) {
		st := NewTheme(Color256).Divider
		row := dividerRow(st, 40)
		assert.True(t, strings.HasPrefix(row, st.Open()), "opens with the style")
		assert.True(t, strings.HasSuffix(row, sgrReset), "resets after the fill")
		body := strings.TrimSuffix(strings.TrimPrefix(row, st.Open()), sgrReset)
		assert.Len(t, body, max(40-1, 0), "one short of width, all spaces carrying the background")
		for _, r := range body {
			assert.Equal(t, ' ', r)
		}
	})

	t.Run("colorless_falls_back_to_rule", func(t *testing.T) {
		st := NewTheme(ColorNone).Divider
		row := dividerRow(st, 10)
		assert.Equal(t, strings.Repeat(ruleChar, 9), row)
	})

	t.Run("zero_width_is_safe", func(t *testing.T) {
		// unknown width falls back to a thin rule rather than an empty band.
		assert.Equal(t, strings.Repeat(ruleChar, 1), dividerRow(NewTheme(Color256).Divider, 0))
	})
}

func TestUIDividerCommitsBand(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	u := &UI{theme: NewTheme(ColorNone), render: &plainRenderer{out: &buf}, mode: ModePlain}
	u.Divider()
	assert.Equal(t, strings.Repeat(ruleChar, max(defaultWidth-1, 1))+"\n", buf.String(),
		"a divider commits a near-full-width line even without color")
}
