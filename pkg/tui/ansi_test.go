package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCursorTo(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "\x1b[1;1H", cursorTo(1, 1))
	assert.Equal(t, "\x1b[24;80H", cursorTo(24, 80))
}

func TestCursorUp(t *testing.T) {
	t.Parallel()

	assert.Empty(t, cursorUp(0))
	assert.Empty(t, cursorUp(-1))
	assert.Equal(t, "\x1b[3A", cursorUp(3))
}

func TestCursorRight(t *testing.T) {
	t.Parallel()

	assert.Empty(t, cursorRight(0))
	assert.Equal(t, "\x1b[5C", cursorRight(5))
}

func TestSgr(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "\x1b[0m", sgr())
	assert.Equal(t, "\x1b[1m", sgr(attrBold))
	assert.Equal(t, "\x1b[2;3;38;5;245m", sgr(attrDim, attrItalic, 38, 5, 245))
}
