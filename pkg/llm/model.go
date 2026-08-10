package llm

import "slices"

// Modality is an input kind a model accepts.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
)

// Model is one addressable model on one provider.
type Model struct {
	Provider      string
	ID            string
	Name          string
	Aliases       []string
	ContextWindow int
	MaxOutput     int
	Input         []Modality
	Caps          Capabilities      // resolved from dialect, provider then model compat
	Headers       map[string]string // per request additions over the provider's
}

// Key returns the canonical provider/id identifier.
func (m Model) Key() string { return m.Provider + "/" + m.ID }

// Accepts reports whether the model takes the given input kind.
func (m Model) Accepts(mod Modality) bool { return slices.Contains(m.Input, mod) }

// Display returns the name to show, falling back to the id.
func (m Model) Display() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}
