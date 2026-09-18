package img

import (
	"encoding/binary"
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// noise returns a w x h image of deterministic pseudo-random pixels, so its
// png encoding stays large enough to exercise the shrink ladder.
func noise(w, h int) *image.RGBA {
	rng := rand.New(rand.NewSource(1))
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			c := color.RGBA{
				R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)),
				B: uint8(rng.Intn(256)), A: 255,
			}
			m.Set(x, y, c)
			m.Set(x+1, y, c)
			m.Set(x, y+1, c)
			m.Set(x+1, y+1, c)
		}
	}
	return m
}

// bmpBytes builds a minimal 24-bit uncompressed BMP of a w x h solid color.
// The stdlib registers no bmp encoder, which is exactly what makes it the
// conversion test's input format.
func bmpBytes(t *testing.T, w, h int) []byte {
	t.Helper()

	rowSize := ((3*w + 3) / 4) * 4
	pixels := rowSize * h
	fileSize := uint32(54 + pixels)
	buf := make([]byte, fileSize)
	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[2:], fileSize)
	binary.LittleEndian.PutUint32(buf[10:], 54)
	binary.LittleEndian.PutUint32(buf[14:], 40)
	binary.LittleEndian.PutUint32(buf[18:], uint32(w))
	binary.LittleEndian.PutUint32(buf[22:], uint32(h))
	binary.LittleEndian.PutUint16(buf[26:], 1)
	binary.LittleEndian.PutUint16(buf[28:], 24)
	for y := 0; y < h; y++ {
		row := 54 + (h-1-y)*rowSize
		for x := 0; x < w; x++ {
			buf[row+3*x], buf[row+3*x+1], buf[row+3*x+2] = 0x20, 0x40, 0x80
		}
	}
	return buf
}
