package tokens

import "sync"

// calibrateAlpha is the EWMA smoothing applied to each reported/predicted ratio.
const calibrateAlpha = 0.3

// Calibrator maintains a per-model correction factor that scales raw estimates
// toward the provider's own counts. Each response feeds it (predicted, reported)
// prompt sizes; the smoothed ratio then multiplies future estimates at read time,
// so a late correction fixes numbers already accumulated.
type Calibrator struct {
	mu      sync.Mutex
	factors map[string]float64 // model key -> multiplier applied to raw estimates
}

// NewCalibrator returns an empty calibrator. It is safe for concurrent use and
// shared by parent and child ledgers of a session.
func NewCalibrator() *Calibrator { return &Calibrator{} }

// Feed records one (predicted, reported) prompt pair for key and updates the
// smoothed factor. The first sample seeds the factor directly; later ones are an
// EWMA so no single outlier can swing a settled value.
func (c *Calibrator) Feed(key string, predicted, reported int) {
	if predicted <= 0 {
		return
	}
	ratio := float64(reported) / float64(predicted)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.factors == nil {
		c.factors = make(map[string]float64)
	}
	prev, ok := c.factors[key]
	var factor float64
	switch {
	case !ok || prev <= 0:
		factor = ratio // first sample seeds directly
	default:
		factor = calibrateAlpha*ratio + (1-calibrateAlpha)*prev
	}
	c.factors[key] = factor
}

// Factor returns the multiplier to apply to a model's raw estimates, or 1 before
// any sample has settled.
func (c *Calibrator) Factor(key string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.factors[key]; ok && f > 0 {
		return f
	}
	return 1
}

// Settled reports whether at least one sample has been recorded for key.
func (c *Calibrator) Settled(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.factors[key]
	return ok && f > 0
}
