package img

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNote(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		res  Result
		want string
	}{
		{"untouched", Result{}, ""},
		{"resized_only", Result{SrcWidth: 10, SrcHeight: 6, Width: 5, Height: 3},
			"[Image: original 10x6, displayed at 5x3. Multiply coordinates by 2 to " +
				"map back to original pixels]"},
		{"converted_only", Result{MediaType: typePNG, Converted: true},
			"[Image: format converted to image/png]"},
		{"flattened_only", Result{Flattened: true},
			"[Image: animated image flattened to its first frame]"},
		{"flattened_frame_count", Result{Flattened: true, Frames: 24},
			"[Image: flattened a 24-frame animation to its first frame]"},
		{"resized_and_flattened", Result{
			SrcWidth: 10, SrcHeight: 6, Width: 5, Height: 3, Flattened: true,
		}, "[Image: original 10x6, displayed at 5x3. Multiply coordinates by 2 to " +
			"map back to original pixels; animated image flattened to its first frame]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.res.Note())
		})
	}
}
