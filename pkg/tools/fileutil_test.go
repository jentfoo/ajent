package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadBytes(t *testing.T) {
	t.Parallel()

	// what a read injects is line-numbered text, so sizing it by the file's raw
	// bytes runs short by the prefix on every line
	m := Measurement{Kind: KindText, Bytes: 20000, Lines: 500}
	assert.Equal(t, int64(20000+500*numberedLinePrefix), ReadBytes(m))

	t.Run("matches_numbered_output", func(t *testing.T) {
		data := []byte(strings.Repeat("fmt.Println(\"hi\")\n", 300))
		out, _, truncated := numberLines(data, 1, ReadFileLimit().Lines)
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
