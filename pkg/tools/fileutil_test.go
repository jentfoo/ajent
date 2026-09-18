package tools

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetect(t *testing.T) {
	t.Parallel()

	var pngBuf, jpegBuf, gifBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	require.NoError(t, png.Encode(&pngBuf, img))
	require.NoError(t, jpeg.Encode(&jpegBuf, img, nil))
	require.NoError(t, gif.Encode(&gifBuf, img, nil))

	wav := []byte("RIFF\x24\x08\x00\x00WAVEfmt \x10\x00\x00\x00")
	wav = append(wav, make([]byte, 16)...)
	// minimal 24-bit bmp of 4x4: file header plus BITMAPINFOHEADER, zero pixels
	bmp := make([]byte, 102)
	bmp[0], bmp[1] = 'B', 'M'
	bmp[2] = 102
	bmp[10] = 54
	bmp[14] = 40
	bmp[18] = 4
	bmp[22] = 4
	bmp[26] = 1
	bmp[28] = 24

	for _, tc := range []struct {
		name string
		data []byte
		want fileKind
	}{
		{"png", pngBuf.Bytes(), fileImage},
		{"jpeg", jpegBuf.Bytes(), fileImage},
		{"gif", gifBuf.Bytes(), fileImage},
		{"bmp", bmp, fileImage},
		{"text looking like bmp", []byte("BMW owners manual, a fine automobile\\nwith more lines"), fileText},
		{"wav is binary", wav, fileBinary},
		{"plain text", []byte("just some text"), fileText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, detect(tc.data))
		})
	}
}

func TestReadBytes(t *testing.T) {
	t.Parallel()

	// what a read injects is line-numbered text, so sizing it by the file's raw
	// bytes runs short by the prefix on every line
	m := Measurement{Kind: KindText, Bytes: 20000, Lines: 500}
	assert.Equal(t, int64(20000+500*numberedLinePrefix), ReadBytes(m))

	t.Run("matches_numbered_output", func(t *testing.T) {
		data := []byte(strings.Repeat("fmt.Println(\"hi\")\n", 300))
		out, _, truncated, _ := numberLines(data, 1, ReadFileLimit().Lines, 0)
		require.Zero(t, truncated)
		assert.Equal(t, int64(len(out)), ReadBytes(Measurement{
			Kind: KindText, Bytes: int64(len(data)), Lines: 300,
		}))
	})

	t.Run("clamped_to_line_limit", func(t *testing.T) {
		// the tool stops at its line limit, so the reserve must not keep counting
		over := Measurement{Kind: KindText, Bytes: 100, Lines: ReadFileLimit().Lines * 4}
		capped := Measurement{Kind: KindText, Bytes: 100, Lines: ReadFileLimit().Lines}
		assert.Equal(t, ReadBytes(capped), ReadBytes(over))
	})

	t.Run("uncounted_lines_fall_back_to_bytes", func(t *testing.T) {
		// Measure skips counting lines above its ceiling
		assert.Equal(t, int64(9000), ReadBytes(Measurement{Kind: KindText, Bytes: 9000}))
	})
}
