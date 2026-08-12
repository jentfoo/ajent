package tokens

import "github.com/jentfoo/ajent/pkg/llm"

// defaultReserveFraction is the fraction of a model's window held back for a
// response when no contextReserve configures it.
const defaultReserveFraction = 0.2

// defaultCompactFraction is where an automatic compaction fires as a fraction of
// the raw window when no compactThreshold configures it.
const defaultCompactFraction = 0.8

// ContextState describes how full the next request will be, exact after a
// response and estimated while one streams or between responses.
type ContextState struct {
	Used      int // tokens the next request's input occupies
	Window    int // the model's raw context window; 0 when unknown
	Reserve   int // tokens held back from Window for the response
	Compact   int // where an automatic compaction would fire; 0 when unset
	Estimated bool
}

// Budget returns the usable input budget, Window minus Reserve. It never goes
// below zero so a misconfigured reserve cannot produce a negative bar.
func (c ContextState) Budget() int {
	if b := c.Window - c.Reserve; b > 0 {
		return b
	}
	return 0
}

// Reserve returns the tokens held back from m's window for its response. A value
// >= 1 is an absolute token count; a value in (0,1) is a fraction of the window;
// anything else uses the default fraction. It clamps to at most 90% of the window,
// and to at least one token.
func Reserve(m llm.Model) int {
	window := m.ContextWindow
	if window <= 0 {
		return 0
	}
	maxReserve := max(1, window*9/10)
	switch {
	case m.ContextReserve >= 1: // absolute token count, clamped to the cap
		r := int(m.ContextReserve)
		if r > maxReserve {
			r = maxReserve
		}
		return r
	default:
		fraction := defaultReserveFraction
		if m.ContextReserve > 0 && m.ContextReserve < 1 { // fraction of the window
			fraction = m.ContextReserve
		}
		r := int(float64(window) * fraction)
		return min(max(r, 1), maxReserve)
	}
}

// CompactAt returns where an automatic compaction fires for m: >= 1 is an
// absolute token count, a value in (0,1) a fraction of the window, anything else
// uses defaultCompactFraction. Never triggers on a zero or negative window.
func CompactAt(m llm.Model) int {
	window := m.ContextWindow
	if window <= 0 {
		return 0
	}
	switch {
	case m.CompactThreshold >= 1: // absolute token count, clamped to the window
		return min(int(m.CompactThreshold), window)
	default:
		fraction := defaultCompactFraction
		if m.CompactThreshold > 0 && m.CompactThreshold < 1 { // fraction of the window
			fraction = m.CompactThreshold
		}
		return int(float64(window) * fraction)
	}
}
