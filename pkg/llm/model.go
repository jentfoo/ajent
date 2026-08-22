package llm

import (
	"slices"
	"strings"
)

// defaultReserveFraction is the fraction of a model's window held back for a
// response when no contextReserve configures it.
const defaultReserveFraction = 0.2

// Modality is an input kind a model accepts.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
)

// Model is one addressable model on one provider.
type Model struct {
	Provider string
	ID       string
	Name     string
	Aliases  []string
	// ContextWindow is the full input window in tokens; 0 when unknown.
	ContextWindow int
	MaxOutput     int
	// ContextReserve is the fraction (or absolute count, >= 1) of the window held
	// back for a response. Values < 1 are fractions of ContextWindow.
	ContextReserve float64
	// CompactThreshold is where an automatic compaction fires: a fraction (<1)
	// or absolute token count (>=1) of ContextWindow; 0 uses the default 0.8.
	CompactThreshold float64
	Input            []Modality
	Caps             Capabilities      // resolved from dialect, provider then model compat
	Headers          map[string]string // per request additions over the provider's
}

// Key returns the canonical provider/id identifier.
func (m Model) Key() string { return m.Provider + "/" + m.ID }

// ShortName is the part of Key after the last slash; falls back to Key when it has none.
func (m Model) ShortName() string {
	if i := strings.LastIndexByte(m.Key(), '/'); i >= 0 && i+1 < len(m.Key()) {
		return m.Key()[i+1:]
	}
	return m.Key()
}

// Accepts reports whether the model takes the given input kind.
func (m Model) Accepts(mod Modality) bool { return slices.Contains(m.Input, mod) }

// Display returns the name to show, falling back to the id.
func (m Model) Display() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// Reserve returns the tokens held back from m's window for its response. A value
// >= 1 is an absolute token count; a value in (0,1) is a fraction of the window;
// anything else uses the default fraction. It clamps to at most 90% of the window,
// and to at least one token.
func (m Model) Reserve() int {
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
