package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeasure(t *testing.T) {
	t.Parallel()

	// a small text file measures its lines and bytes and classifies as text.
	t.Run("text_file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.go")
		content := "package a\n\nfunc f() {}" // 3 lines, no trailing newline
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))

		m, err := Measure(p)
		require.NoError(t, err)
		assert.False(t, m.Dir)
		assert.Equal(t, KindText, m.Kind)
		assert.Equal(t, 3, m.Lines)
		assert.Equal(t, int64(len(content)), m.Bytes)
	})

	// a directory reports Dir and leaves bytes zero.
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		inner := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(inner, 0o700))

		m, err := Measure(inner)
		require.NoError(t, err)
		assert.True(t, m.Dir)
		assert.Zero(t, m.Bytes)
		assert.Zero(t, m.Lines)
	})

	// a file with a NUL byte classifies as binary without counting lines.
	t.Run("binary", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "blob")
		require.NoError(t, os.WriteFile(p, []byte{0x00, 0x01, 0x02, 'h', 'i'}, 0o600))

		m, err := Measure(p)
		require.NoError(t, err)
		assert.False(t, m.Dir)
		assert.Equal(t, KindBinary, m.Kind)
		assert.Zero(t, m.Lines) // binary files do not count lines
		assert.Equal(t, int64(5), m.Bytes)
	})

	// a file above MeasureCeiling reports its bytes and text kind but never reads the whole file to count lines.
	t.Run("large_file_skips_line_count", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "big.txt")
		// just over MeasureCeiling of printable text so it classifies as text
		size := int(MeasureCeiling) + 1
		require.NoError(t, os.WriteFile(p, []byte(repeatByte('a', size)), 0o600))

		m, err := Measure(p)
		require.NoError(t, err)
		assert.Equal(t, KindText, m.Kind)
		assert.Equal(t, int64(size), m.Bytes)
		assert.Zero(t, m.Lines) // a giant file is not read to count lines
	})

	// a missing path surfaces the stat error.
	t.Run("missing", func(t *testing.T) {
		_, err := Measure(filepath.Join(t.TempDir(), "nope"))
		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})
}

// repeatByte returns a string of n copies of b, for a large text fixture.
func repeatByte(b byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = b
	}
	return string(buf)
}
