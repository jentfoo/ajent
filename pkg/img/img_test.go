package img

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
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

// exifJpeg wraps a jpeg with an APP1 EXIF block carrying the given orientation
// tag, little-endian byte order and IFD0 at TIFF offset 8.
func exifJpeg(t *testing.T, src []byte, orient uint16) []byte {
	t.Helper()
	require.LessOrEqual(t, len(src), 1<<14, "jpeg must fit one APP1 segment")
	return app1Jpeg(src, exifPayload(binary.LittleEndian, orient))
}

// exifPayload builds an "Exif\0\0" APP1 payload: a TIFF header with a single
// orientation tag in IFD0, written in the given byte order.
func exifPayload(bo binary.ByteOrder, orient uint16) []byte {
	var tiff bytes.Buffer
	if bo == binary.LittleEndian {
		tiff.Write([]byte{'I', 'I'})
	} else {
		tiff.Write([]byte{'M', 'M'})
	}
	putU16(&tiff, bo, 42)     // magic value
	putU32(&tiff, bo, 8)      // IFD offset pointer -> byte 8
	putU16(&tiff, bo, 1)      // one tag follows
	putU16(&tiff, bo, 0x0112) // orientation tag id
	putU16(&tiff, bo, 3)      // SHORT type
	putU32(&tiff, bo, 1)      // count of one
	putU16(&tiff, bo, orient)
	tiff.Write([]byte{0x00, 0x00}) // pad the entry to twelve bytes
	return append([]byte("Exif\x00\x00"), tiff.Bytes()...)
}

// app1Jpeg splices APP1 segments carrying the payloads after a jpeg's SOI.
func app1Jpeg(src []byte, payloads ...[]byte) []byte {
	var out bytes.Buffer
	out.Write(src[:2]) // SOI marker
	for _, payload := range payloads {
		out.Write([]byte{0xff, 0xe1}) // APP1 marker
		// segment length includes its own two-byte field
		putU16(&out, binary.BigEndian, uint16(len(payload)+2))
		out.Write(payload)
	}
	out.Write(src[2:])
	return out.Bytes()
}

func putU16(buf *bytes.Buffer, bo binary.ByteOrder, v uint16) {
	var b [2]byte
	bo.PutUint16(b[:], v)
	buf.Write(b[:])
}

func putU32(buf *bytes.Buffer, bo binary.ByteOrder, v uint32) {
	var b [4]byte
	bo.PutUint32(b[:], v)
	buf.Write(b[:])
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
			{"png", pngBytes(t, solid(4, 4)), typePNG},
			{"jpeg", jpegBytes(t, solid(4, 4), 80), typeJPEG},
			{"gif", gifBytes.Bytes(), typeGIF},
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
	assert.Equal(t, typePNG, res.MediaType)
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
	assert.Equal(t, typePNG, res.MediaType)
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
	assert.Equal(t, typePNG, res.MediaType)
	assert.Equal(t, 40, res.Width)
	assert.Equal(t, 30, res.Height)
	assert.True(t, res.Flattened)
	assert.Contains(t, res.Note(), "flattened to its first frame")
	assert.False(t, animatedPNG(res.Data, res.MediaType))
	assert.False(t, animatedPNG(pngBytes(t, solid(4, 4)), typePNG)) // plain png untouched
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

func TestPrepareJPEGFallbackFlattensAlpha(t *testing.T) {
	if testing.Short() {
		t.Skip("-short mode")
	}
	t.Parallel()

	// a transparent png too big for the ceiling falls to the jpeg arm, which
	// keeps no alpha: compositing over white must win over whatever rgb sat beneath
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
	assert.Equal(t, typeJPEG, res.MediaType)

	decoded, err := jpeg.Decode(bytes.NewReader(res.Data))
	require.NoError(t, err)
	var sum int64
	for i := range 100 {
		r, _, _, _ := decoded.At(i*7, i*11).RGBA()
		sum += int64(r)
	}
	assert.Greater(t, sum/int64(100), int64(30000)) // mid-gray: white shows through
}

func TestPrepareOversizeJpegShrinks(t *testing.T) {
	t.Parallel()

	// a sendable jpeg over only the pixel ceiling must still be resized, not
	// passed through: withinLimits is checked on both ceilings or none
	res, err := Prepare(jpegBytes(t, solid(2600, 300), 95))
	require.NoError(t, err)
	assert.Equal(t, MaxPixels, res.Width)
	assert.Equal(t, 231, res.Height)
	assert.Equal(t, 2600, res.SrcWidth)
	assert.Equal(t, 300, res.SrcHeight)
	assert.True(t, res.Resized())
}

func TestPrepareAppliesExifOrientation(t *testing.T) {
	if testing.Short() {
		t.Skip("-short mode")
	}
	t.Parallel()

	// a wide jpeg tagged rotate-90: after orientation the honest source size
	// swaps axes, and the re-encoded result reflects the rotated bounds.
	src := exifJpeg(t, jpegBytes(t, solid(2600, 300), 95), 8) // 8 = rotate 90 ccw
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.NotEmpty(t, res.MediaType)
	// oriented source is 300 wide x 2000 tall; the shrink keeps that aspect
	assert.LessOrEqual(t, res.Width, MaxPixels)
	assert.LessOrEqual(t, res.Height, MaxPixels)
	assert.InDelta(t,
		float64(300)/float64(2600), float64(res.SrcWidth)/float64(res.SrcHeight), 0.01)

	// the raw tag read itself must surface rotate-90 and refuse nonsense
	assert.Equal(t, orientationRotate90, jpegOrientation(src))
}

func TestJPEGOrientation(t *testing.T) {
	t.Parallel()

	fake := []byte{0xff, 0xd8, 0xff, 0xd9} // SOI + EOI, all the scan needs
	xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x/>")
	badOrder := append([]byte("Exif\x00\x00"), 0x00, 0x2a, 0x00, 0x00, 0x00, 0x08, 0x00, 0x01)

	for _, c := range []struct {
		name string
		data []byte
		want int
	}{
		{"plain_jpeg", jpegBytes(t, solid(4, 3), 80), 0},
		{"garbage_input", []byte("not a jpeg at all"), 0},
		{"little_endian", app1Jpeg(fake, exifPayload(binary.LittleEndian, orientationRotate90)), orientationRotate90},
		{"big_endian", app1Jpeg(fake, exifPayload(binary.BigEndian, orientationRotate270)), orientationRotate270},
		{"xmp_before_exif", app1Jpeg(fake, xmp, exifPayload(binary.LittleEndian, orientationRotate90)), orientationRotate90},
		{"xmp_only", app1Jpeg(fake, xmp), 0},
		{"unknown_byte_order", app1Jpeg(fake, badOrder), 0},
		{"truncated_tiff", app1Jpeg(fake, exifPayload(binary.LittleEndian, 1)[:9]), 0},
		{"orientation_out_of_range", app1Jpeg(fake, exifPayload(binary.LittleEndian, 9)), 0},
		{"ifd_outside_block", app1Jpeg(fake, append(exifPayload(binary.LittleEndian, 1)[:8], 0xff, 0xff, 0xff, 0xff, 0x00, 0x01)), 0},
		{"segment_size_overrun", []byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff, 'E', 'x', 'i', 'f'}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, jpegOrientation(c.data))
		})
	}
}

func TestFixOrientation(t *testing.T) {
	t.Parallel()

	// 3x2 source with distinct pixel values 1..6
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			v := uint8(3*y + x + 1)
			src.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	gray := func(v uint8) color.NRGBA { return color.NRGBA{R: v, G: v, B: v, A: 255} }

	for _, c := range []struct {
		name string
		tag  int
		want [][]uint8 // expected output rows, hard-coded from the EXIF spec
	}{
		{"no_transform", orientationNormal, [][]uint8{{1, 2, 3}, {4, 5, 6}}},
		{"flip_horizontal", orientationFlipH, [][]uint8{{3, 2, 1}, {6, 5, 4}}},
		{"rotate_180", orientationRotate180, [][]uint8{{6, 5, 4}, {3, 2, 1}}},
		{"flip_vertical", orientationFlipV, [][]uint8{{4, 5, 6}, {1, 2, 3}}},
		{"transpose", orientationTranspose, [][]uint8{{1, 4}, {2, 5}, {3, 6}}},
		{"rotate_90_cw", orientationRotate270, [][]uint8{{4, 1}, {5, 2}, {6, 3}}},
		{"transverse", orientationTransverse, [][]uint8{{6, 3}, {5, 2}, {4, 1}}},
		{"rotate_90_ccw", orientationRotate90, [][]uint8{{3, 6}, {2, 5}, {1, 4}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := fixOrientation(src, c.tag)
			require.Equal(t, len(c.want), got.Bounds().Dy())
			require.Equal(t, len(c.want[0]), got.Bounds().Dx())
			for y, row := range c.want {
				for x, v := range row {
					assert.Equal(t, gray(v), got.NRGBAAt(x, y))
				}
			}
		})
	}
}
