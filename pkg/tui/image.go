package tui

import (
	"github.com/jentfoo/ajent/pkg/llm"
)

// Image is one image a caller wants committed to history.
type Image struct {
	Data  []byte // encoded image bytes
	Label string // short origin label for the placeholder, e.g. a path
}

// String returns the text stand-in naming this image, the single placeholder
// form shared by transcript replay and MCP display.
func (im Image) String() string { return llm.ImagePlaceholder(im.Data, im.Label) }
