package tokens

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalibratorConverges(t *testing.T) {
	t.Parallel()

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
}

func TestCalibratorFirstSampleSeedsDirectly(t *testing.T) {
	t.Parallel()

	c := NewCalibrator()
	const key = "p/m"

	identity := c.Factor(key) // unsampled: the identity factor (one)
	assert.InDelta(t, 1.0, identity, 1e-9)

	// a single sample with ratio three moves the factor to it immediately.
	c.Feed(key, 100, 300)
	assert.InDelta(t, 3.0, c.Factor(key), 1e-9) // first sample seeds directly
}

func TestCalibratorIgnoresZeroPrediction(t *testing.T) {
	t.Parallel()

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
}
