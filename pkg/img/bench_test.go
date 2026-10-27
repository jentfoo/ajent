package img

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchJPEG encodes a screenshot-shaped photo (3000x2000) once, so Prepare
// measures the fit path and not fixture encoding.
func benchJPEG(b *testing.B) []byte {
	b.Helper()

	const w, h = 3000, 2000
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(b, jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85}))
	return buf.Bytes()
}

// BenchmarkImagePrepare measures the per-attach fit: decode, EXIF orientation,
// resampling down to the inline limit and re-encode.
func BenchmarkImagePrepare(b *testing.B) {
	big := benchJPEG(b)

	res, err := Prepare(big)
	require.NoError(b, err)
	require.True(b, res.Resized())
	b.ReportMetric(float64(len(big)), "src-B/op")

	for b.Loop() {
		if _, err := Prepare(big); err != nil {
			b.Fatal(err)
		}
	}
}
