package img

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// solid returns a w x h RGBA image filled with one color.
func solid(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	// fill Pix directly: per-pixel Set is pure cost on multi-megapixel sources
	c := []byte{200, 30, 40, 255}
	copy(m.Pix, c)
	for n := len(c); n < len(m.Pix); n *= 2 {
		copy(m.Pix[n:], m.Pix[:n])
	}
	return m
}

// pngBytes encodes m as png.
func pngBytes(t *testing.T, m image.Image) []byte {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

// jpegBytes encodes m as jpeg.
func jpegBytes(t *testing.T, m image.Image, q int) []byte {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, m, &jpeg.Options{Quality: q}))
	return b.Bytes()
}

func TestSniff(t *testing.T) {
	t.Parallel()
	gifBytes := &bytes.Buffer{}
	require.NoError(t, gif.Encode(gifBytes, solid(4, 4), nil))

	t.Run("known formats", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name string
			data []byte
			want string
		}{
			{"png", pngBytes(t, solid(4, 4)), TypePNG},
			{"jpeg", jpegBytes(t, solid(4, 4), 80), TypeJPEG},
			{"gif", gifBytes.Bytes(), TypeGIF},
		} {
			mt, ok := Sniff(c.data)
			assert.True(t, ok, c.name)
			assert.Equal(t, c.want, mt, c.name)
		}
	})

	t.Run("rejects text", func(t *testing.T) {
		t.Parallel()
		_, ok := Sniff([]byte("just some text, not an image"))
		assert.False(t, ok)
	})
}

func TestPreparePassthrough(t *testing.T) {
	t.Parallel()
	src := pngBytes(t, solid(120, 80))
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.Equal(t, TypePNG, res.MediaType)
	assert.Equal(t, src, res.Data)
	assert.Equal(t, 120, res.Width)
	assert.Equal(t, 80, res.Height)
	assert.False(t, res.Resized())
	assert.False(t, res.Converted)
	assert.Empty(t, res.Note())
}

func TestPrepareDownscalesUnderCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("-short mode")
	}
	t.Parallel()

	// a noisy large png: noise defeats compression, pushing the encoded size
	// past the ceiling so the jpeg arm of the ladder does real work
	src := pngBytes(t, noise(2600, 2000))
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.LessOrEqual(t, res.Width, MaxPixels)
	assert.LessOrEqual(t, res.Height, MaxPixels)
	enc := base64.StdEncoding.EncodedLen(len(res.Data))
	assert.LessOrEqual(t, enc, MaxBase64Bytes)
	assert.Equal(t, 2600, res.SrcWidth)
	assert.Equal(t, 2000, res.SrcHeight)
	assert.True(t, res.Resized())
	assert.NotEmpty(t, res.Note())
	assert.Contains(t, res.Note(), "original 2600x2000")
	assert.Contains(t, res.Note(), "displayed at")
	// both axes scale together, so the note's single factor is honest
	ratio := float64(res.Width) / float64(res.Height)
	assert.InDelta(t, 2600.0/2000.0, ratio, 0.01)
}

func TestPrepareKeepsAspect(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"ultrawide screenshot", 3840, 1080, 2000, 563},
		{"tall phone capture", 1080, 2400, 900, 2000},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := Prepare(pngBytes(t, solid(c.w, c.h)))
			require.NoError(t, err)
			assert.Equal(t, c.wantW, res.Width)
			assert.Equal(t, c.wantH, res.Height)
			assert.Equal(t, c.w, res.SrcWidth)
			assert.Equal(t, c.h, res.SrcHeight)
		})
	}
}

func TestPrepareConvertsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	// bmp has no stdlib encoder; hand-build a minimal 24-bit one
	src := bmpBytes(t, 4, 3)
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.Equal(t, TypePNG, res.MediaType)
	assert.Equal(t, 4, res.Width)
	assert.Equal(t, 3, res.Height)
	assert.False(t, res.Resized())
	assert.True(t, res.Converted)
	assert.Contains(t, res.Note(), "format converted")
}

// pngChunk builds one png chunk with its CRC: length, type, data, crc.
func pngChunk(typ string, data []byte) []byte {
	out := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(out, uint32(len(data)))
	copy(out[4:], typ)
	copy(out[8:], data)
	crc := crc32.ChecksumIEEE(out[4 : 8+len(data)])
	binary.BigEndian.PutUint32(out[8+len(data):], crc)
	return out
}

// apngBytes wraps a png's bytes with an animation control chunk, the way an
// APNG encoder would: acTL rides between IHDR and the first IDAT.
func apngBytes(t *testing.T, m image.Image) []byte {
	t.Helper()

	src := pngBytes(t, m)
	var out bytes.Buffer
	out.Write(src[:8]) // signature
	out.Write(pngChunk("acTL", make([]byte, 8)))
	out.Write(src[8:])
	return out.Bytes()
}

func TestPrepareConvertsAnimatedPNG(t *testing.T) {
	t.Parallel()
	// an animated png within the limits must not ride to the provider
	// verbatim: it is re-encoded to its first frame, with the flattening
	// disclosed in the note so the model knows it sees one frame
	src := apngBytes(t, solid(40, 30))
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.Equal(t, TypePNG, res.MediaType)
	assert.Equal(t, 40, res.Width)
	assert.Equal(t, 30, res.Height)
	assert.True(t, res.Flattened)
	assert.Contains(t, res.Note(), "flattened to its first frame")
	assert.False(t, animatedPNG(res.Data, res.MediaType))
	assert.False(t, animatedPNG(pngBytes(t, solid(4, 4)), TypePNG)) // plain png untouched
}

func TestPrepareRejectsVastDimensions(t *testing.T) {
	t.Parallel()
	// a valid header claiming far more pixels than any decode may allocate
	// for is refused before the bytes are ever decoded
	src := pngBytes(t, solid(1, 1))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 30000)
	binary.BigEndian.PutUint32(ihdr[4:], 30000)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // RGBA, matching the source pixel
	var b bytes.Buffer
	b.Write(src[:8])
	b.Write(pngChunk("IHDR", ihdr))
	b.Write(src[33:]) // everything after the original IHDR

	_, err := Prepare(b.Bytes())
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestPrepareRejectsGarbage(t *testing.T) {
	t.Parallel()
	_, err := Prepare([]byte("definitely not an image"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupported)
}

// a transparent png too big for the ceiling falls to the jpeg arm, which
// keeps no alpha: compositing over white must win over whatever rgb sat beneath
func TestPrepareJPEGFallbackFlattensAlpha(t *testing.T) {
	if testing.Short() {
		t.Skip("-short mode")
	}
	t.Parallel()

	// black rgb under noisy alpha: the png arm busts the ceiling (noise will
	// not compress), and the jpeg arm would encode solid black if the alpha
	// were dropped rather than composited over white
	m := image.NewNRGBA(image.Rect(0, 0, 2600, 2000))
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < len(m.Pix); i += 4 {
		m.Pix[i+3] = uint8(rng.Intn(256))
	}
	res, err := Prepare(pngBytes(t, m))
	require.NoError(t, err)
	assert.Equal(t, TypeJPEG, res.MediaType)

	decoded, err := jpeg.Decode(bytes.NewReader(res.Data))
	require.NoError(t, err)
	var sum int64
	for i := range 100 {
		r, _, _, _ := decoded.At(i*7, i*11).RGBA()
		sum += int64(r)
	}
	assert.Greater(t, sum/int64(100), int64(30000)) // mid-gray: white shows through
}

// a sendable jpeg over only the pixel ceiling must still be resized, not
// passed through: withinLimits is checked on both ceilings or none
func TestPrepareOversizeJpegShrinks(t *testing.T) {
	t.Parallel()

	res, err := Prepare(jpegBytes(t, solid(2600, 300), 95))
	require.NoError(t, err)
	assert.Equal(t, MaxPixels, res.Width)
	assert.Equal(t, 231, res.Height)
	assert.Equal(t, 2600, res.SrcWidth)
	assert.Equal(t, 300, res.SrcHeight)
	assert.True(t, res.Resized())
}
