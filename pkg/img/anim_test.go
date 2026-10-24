package img

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apngBytes wraps a png's bytes with an acTL announcing n frames, the way an
// APNG encoder would: acTL rides between IHDR and the first IDAT.
func apngBytes(t *testing.T, m image.Image, n uint32) []byte {
	t.Helper()

	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl, n)
	src := pngBytes(t, m)
	var out bytes.Buffer
	out.Write(src[:8])
	out.Write(pngChunk("acTL", actl))
	out.Write(src[8:])
	return out.Bytes()
}

func TestAPNGFrames(t *testing.T) {
	t.Parallel()

	m := solid(8, 6)

	n2, ok := apngFrames(pngBytes(t, m))
	assert.False(t, ok)
	assert.Zero(t, n2)

	// the count rides in the acTL payload; a declared zero still reads animated
	n2, ok = apngFrames(apngBytes(t, m, 5))
	assert.True(t, ok)
	assert.Equal(t, 5, n2)

	n2, ok = apngFrames(apngBytes(t, m, 0))
	assert.True(t, ok)
	assert.Equal(t, 1, n2)

	// a count declared but not fully present reads as animated, count unknown:
	// sig and acTL header with 2 of the 8 payload bytes
	n2, ok = apngFrames(apngBytes(t, m, 5)[:18])
	assert.True(t, ok)
	assert.Equal(t, 1, n2)

	// every truncation of a valid file must fail closed without panicking: exact
	// capacity slices, so a read past len cannot hide in spare capacity
	anim := apngBytes(t, m, 5)
	for i := range anim {
		frames, animated := apngFrames(append(make([]byte, 0, i), anim[:i]...))
		if animated {
			assert.GreaterOrEqual(t, frames, 1)
		}
	}
}

func TestScanGIF(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name   string
		data   []byte
		frames int
		ok     bool
	}{
		{"nil", nil, 0, false},
		{"garbage_header", []byte("GIF89b\x00\x00\x01\x00\x01\x00\x00"), 0, false},
		{"header_only", gifFrames(t, 8, 6, 1)[:13], 0, false},
		{"single_frame", gifFrames(t, 8, 6, 1), 1, true},
		{"two_frames", gifFrames(t, 8, 6, 2), 2, true},
		{"three_frames", gifFrames(t, 8, 6, 3), 3, true},
		// EncodeAll never emits a global color table but real exports do: the walk
		// must skip its bytes rather than read them as blocks
		{"global_table_single", withGCT(gifFrames(t, 8, 6, 1)), 1, true},
		{"global_table_two", withGCT(gifFrames(t, 8, 6, 2)), 2, true},
		// a table the header promises but the file does not hold
		{"global_table_overrun", append([]byte("GIF89a"), 0x01, 0x00, 0x01, 0x00, 0x87, 0x00, 0x00), 0, false},
		// an introducer the spec does not define is refuse-able: decode fails too
		{"unknown_block", append([]byte("GIF87a"), 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff), 0, false},
		// junk past the trailer: every frame is already counted
		{"trailing_junk", append(gifFrames(t, 8, 6, 2), 0x00, 0x41, 0x42), 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			frames, ok := scanGIF(c.data)
			assert.Equal(t, c.frames, frames)
			assert.Equal(t, c.ok, ok)
		})
	}

	// gif.Encode writes a global color table: stdlib output must parse cleanly
	t.Run("stdlib_encoded", func(t *testing.T) {
		var b bytes.Buffer
		require.NoError(t, gif.Encode(&b, solid(8, 6), nil))
		frames, ok := scanGIF(b.Bytes())
		assert.Equal(t, 1, frames)
		assert.True(t, ok)
	})

	// every truncation of a valid file must fail closed without panicking: the
	// unwalkable tail may hide more frames than were counted
	t.Run("truncated_prefixes", func(t *testing.T) {
		anim := gifFrames(t, 8, 6, 2)
		for i := range anim {
			_, ok := scanGIF(anim[:i])
			assert.False(t, ok)
		}
	})

	// a fitting plain png is not a gif and must not read as one
	t.Run("png_passthrough", func(t *testing.T) {
		frames, ok := scanGIF(pngBytes(t, solid(8, 6)))
		assert.Zero(t, frames)
		assert.False(t, ok)
	})
}

// animatedWebPFixture loads the two-frame test webp and its extracted leading
// frame as a plain still.
func animatedWebPFixture(t *testing.T) (src, still []byte) {
	t.Helper()

	src, err := os.ReadFile("testdata/animated.webp")
	require.NoError(t, err)
	frames, s := webpAnimation(src)
	require.Equal(t, 2, frames)
	require.NotNil(t, s)
	return src, s
}

// buildAnimatedWebP assembles a webp from raw chunks: the canvas rides in VP8X
// with the animation flag set, then one ANMF payload per frame.
func buildAnimatedWebP(t *testing.T, w, h int, anmf ...[]byte) []byte {
	t.Helper()

	vp8x := make([]byte, 10)
	vp8x[0] = webpAnimationBit
	putUint24(vp8x[4:7], uint32(w-1))
	putUint24(vp8x[7:10], uint32(h-1))

	chunks := []byte("WEBP") // the fourCC plus each chunk: the RIFF body
	chunks = append(chunks, riffChunk("VP8X", vp8x)...)
	chunks = append(chunks, riffChunk("ANIM", make([]byte, 6))...)
	for _, p := range anmf {
		chunks = append(chunks, riffChunk("ANMF", p)...)
	}

	out := binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(chunks)))
	return append(out, chunks...)
}

// anmfPayload wraps one frame's bitstream chunk in the 16-byte frame header
// the extractor skips.
func anmfPayload(still []byte) []byte {
	return append(make([]byte, 16), still[12:]...)
}

// riffChunk frames one payload as a RIFF chunk, padded to an even size.
func riffChunk(fourCC string, payload []byte) []byte {
	out := binary.LittleEndian.AppendUint32([]byte(fourCC), uint32(len(payload)))
	out = append(out, payload...)
	if len(payload)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

// putUint24 writes v as three little-endian bytes.
func putUint24(b []byte, v uint32) {
	b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16)
}

func TestWebPAnimation(t *testing.T) {
	t.Parallel()

	src, still := animatedWebPFixture(t)

	// the fixture carries two frames; its leading frame re-sniffs as a plain webp
	frames, s := webpAnimation(src)
	assert.Equal(t, 2, frames)
	require.NotNil(t, s)
	mt, cfg, ok := sniff(s)
	assert.True(t, ok)
	assert.Equal(t, typeWebP, mt)
	assert.Equal(t, 8, cfg.Width)

	// the still is a self-contained file: its RIFF size field counts its own bytes
	assert.Equal(t, len(s)-8, int(binary.LittleEndian.Uint32(s[4:8])))

	// an extracted still is not animated
	frames, s = webpAnimation(still)
	assert.Zero(t, frames)
	assert.Nil(t, s)

	// the flag promises animation no chunk shows: one frame, nothing to extract
	bare := buildAnimatedWebP(t, 8, 6)
	frames, s = webpAnimation(bare)
	assert.Equal(t, 1, frames)
	assert.Nil(t, s)

	// short garbage reads as a still
	frames, s = webpAnimation([]byte("RIFF"))
	assert.Zero(t, frames)
	assert.Nil(t, s)
}

func TestPrepareFlattensAnimatedWebP(t *testing.T) {
	t.Parallel()

	src, _ := animatedWebPFixture(t)

	// animation never rides through: the still is re-encoded like any other
	// fitting image that cannot pass verbatim
	res, err := Prepare(src)
	require.NoError(t, err)
	assert.Equal(t, typePNG, res.MediaType)
	assert.True(t, res.Flattened)
	assert.Equal(t, 2, res.Frames)
	assert.Contains(t, res.Note(), "flattened a 2-frame animation to its first frame")
}

func TestPrepareAnimatedWebPOversizeCanvas(t *testing.T) {
	t.Parallel()

	// a canvas over the pixel ceiling does not refuse the file: only the
	// extracted still has to fit
	_, still := animatedWebPFixture(t)
	res, err := Prepare(buildAnimatedWebP(t, 2100, 10, anmfPayload(still), anmfPayload(still)))
	require.NoError(t, err)
	assert.Equal(t, typePNG, res.MediaType)
	assert.True(t, res.Flattened)
	assert.Equal(t, 2, res.Frames)
}

func TestPrepareAnimatedWebPUnreadable(t *testing.T) {
	t.Parallel()

	// the flag promises animation nothing decodable shows: refused, since a
	// fitting one must not ride through as an animation either
	_, err := Prepare(buildAnimatedWebP(t, 8, 6))
	assert.ErrorIs(t, err, ErrUnsupported)
}

func TestPrepareStaticWebPPassthrough(t *testing.T) {
	t.Parallel()

	// the extracted still is a plain webp: it rides through verbatim, no note
	_, still := animatedWebPFixture(t)
	res, err := Prepare(still)
	require.NoError(t, err)
	assert.Equal(t, typeWebP, res.MediaType)
	assert.Equal(t, still, res.Data)
	assert.False(t, res.Flattened)
	assert.Empty(t, res.Note())
}
