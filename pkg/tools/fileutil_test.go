package tools

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, content, 0o644))
		return p
	}

	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	require.NoError(t, png.Encode(&pngBuf, img))

	bin := append(bytes.Repeat([]byte{'a'}, sniffLen-1), 0)   // NUL ends the window
	bin = append(bin, bytes.Repeat([]byte{'b'}, sniffLen)...) // tail beyond it must not matter

	for _, tc := range []struct {
		name     string
		path     string
		wantKind fileKind
		wantData []byte
	}{
		{"small_text", write("small.txt", []byte("plain words\n")), fileText, []byte("plain words\n")},
		{"empty_file", write("empty.txt", nil), fileText, nil},
		{"text_beyond_window", write("big.txt", bytes.Repeat([]byte("abcdefgh\n"), sniffLen)),
			fileText, bytes.Repeat([]byte("abcdefgh\n"), sniffLen)},
		{"exact_window_size", write("edge.txt", bytes.Repeat([]byte{'a'}, sniffLen)),
			fileText, bytes.Repeat([]byte{'a'}, sniffLen)},
		{"binary_carries_no_data", write("bin.dat", bin), fileBinary, nil},
		{"image_returns_full_bytes", write("pic.png", pngBuf.Bytes()), fileImage, pngBuf.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, info, kind, err := probeFile(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, kind)
			if tc.wantData == nil { // no content: none there is, or the binary contract
				assert.Empty(t, data)
			} else {
				assert.Equal(t, tc.wantData, data)
			}
			require.NotNil(t, info)
			if kind != fileBinary { // binary returns no content by contract
				assert.Equal(t, int64(len(tc.wantData)), info.Size())
			}
		})
	}

	t.Run("missing_path_errors", func(t *testing.T) {
		_, _, kind, err := probeFile(filepath.Join(dir, "nope.txt"))
		require.ErrorIs(t, err, fs.ErrNotExist)
		assert.Equal(t, fileText, kind) // error paths default to text
	})

	t.Run("directory_errors", func(t *testing.T) {
		_, _, kind, err := probeFile(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a directory")
		assert.Equal(t, fileText, kind)
	})
}

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

func TestStatRegular(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fi, err := statRegular(filepath.Join(dir, "missing.txt"))
	require.NoError(t, err)
	assert.Nil(t, fi) // a missing file is not refused here

	p := filepath.Join(dir, "reg.txt")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	fi, err = statRegular(p)
	require.NoError(t, err)
	require.NotNil(t, fi)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestWritePerm(t *testing.T) {
	t.Parallel()

	assert.Equal(t, os.FileMode(0o644), writePerm(nil)) // a new file gets the default

	dir := t.TempDir()
	p := filepath.Join(dir, "m.txt")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), writePerm(fi)) // an overwrite keeps the mode
}

func TestReadBytes(t *testing.T) {
	t.Parallel()

	// what a read injects is line-numbered text, so sizing it by the file's raw
	// bytes runs short by the prefix on every line
	m := Measurement{Kind: KindText, Bytes: 20000, Lines: 500}
	assert.Equal(t, int64(20000+500*numberedLinePrefix), ReadBytes(m))

	t.Run("matches_numbered_output", func(t *testing.T) {
		data := []byte(strings.Repeat("fmt.Println(\"hi\")\n", 300))
		out, _, truncated, _ := numberLines(data, 1, ReadFileLimit().Lines, 0, false)
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
