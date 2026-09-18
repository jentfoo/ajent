package img

import (
	"fmt"
	"strings"
)

// Note returns the coordinate-mapping text that accompanies a fitted image:
// the original dimensions, the sent dimensions and the scale factor between
// them, so the model's spatial answers stay correct against the source pixels.
// It also names a format conversion or animation flattening, provenance the
// model should know. An empty string when nothing changed.
func (r Result) Note() string {
	if !r.Resized() && !r.Converted && !r.Flattened {
		return ""
	}
	var clauses []string
	if r.Resized() {
		scale := float64(r.SrcWidth) / float64(max(r.Width, 1))
		clauses = append(clauses, fmt.Sprintf(
			"original %dx%d, displayed at %dx%d. Multiply coordinates by %.4g to map back to original pixels",
			r.SrcWidth, r.SrcHeight, r.Width, r.Height, scale))
	}
	if r.Converted {
		clauses = append(clauses, "format converted to "+r.MediaType)
	}
	if r.Flattened {
		clauses = append(clauses, "animated PNG flattened to its first frame")
	}
	return "[Image: " + strings.Join(clauses, "; ") + "]"
}
