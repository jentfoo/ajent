package tokens

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalibrator(t *testing.T) {
	t.Parallel()

	t.Run("converges", func(t *testing.T) {
		c := NewCalibrator()
		const key = "p/m"

		// feed a steady 2x undercount; the factor settles at exactly that ratio.
		for i := 0; i < 20; i++ {
			c.Feed(key, 1000, 2000)
		}
		assert.InDelta(t, 2.0, c.Factor(key), 1e-6)

		// a single outlier cannot swing a settled factor to its own ratio: EWMA
		// damps the jump well below it.
		settled := c.Factor(key)
		c.Feed(key, 5000, 25000) // one 5x sample (ratio five)
		outlierRatio := float64(25000) / float64(5000)
		after := c.Factor(key)
		assert.Less(t, after, outlierRatio) // pulled toward but far short of the ratio
		assert.Greater(t, after, settled)   // and it did register a move
	})

	t.Run("first_sample_seeds_directly", func(t *testing.T) {
		c := NewCalibrator()
		const key = "p/m"

		identity := c.Factor(key) // unsampled: the identity factor (one)
		assert.InDelta(t, 1.0, identity, 1e-9)

		// a single sample with ratio three moves the factor to it immediately.
		c.Feed(key, 100, 300)
		assert.InDelta(t, 3.0, c.Factor(key), 1e-9) // first sample seeds directly
	})

	t.Run("ignores_zero_prediction", func(t *testing.T) {
		c := NewCalibrator()
		const key = "p/m"
		before := c.Factor(key)
		c.Feed(key, 200, 400) // a real sample first so the pair is usable
		mid := c.Factor(key)
		assert.Greater(t, mid, before) // it did move off identity

		// zero predicted: nothing to ratio against, factor unchanged.
		afterSample := c.Factor(key)
		c.Feed(key, 0, 900000)
		assert.InDelta(t, afterSample, c.Factor(key), 1e-9)
	})

	t.Run("ignores_unreported_turns", func(t *testing.T) {
		c := NewCalibrator()
		c.Feed("k", 1000, 1100)
		settled := c.Factor("k")
		require.InDelta(t, 1.1, settled, 0.001)

		// a provider that reported nothing is not evidence the estimate ran high; before
		// this guard each such turn decayed the factor (1.1, 0.77, 0.539, ...) until every
		// estimate for the model came out far too small.
		c.Feed("k", 1000, 0)
		c.Feed("k", 1000, 0)
		assert.InDelta(t, settled, c.Factor("k"), 1e-9)
	})
}
