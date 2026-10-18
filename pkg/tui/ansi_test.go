package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCursorTo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		row, col int
		want     string
	}{
		{name: "origin", row: 1, col: 1, want: "\x1b[1;1H"},
		{name: "bottom_right", row: 24, col: 80, want: "\x1b[24;80H"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cursorTo(tc.row, tc.col))
		})
	}
}

func TestSgr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []int
		want string
	}{
		{name: "reset", args: nil, want: "\x1b[0m"},
		{name: "single_attr", args: []int{attrBold}, want: "\x1b[1m"},
		{name: "attrs_and_color_params", args: []int{attrDim, attrItalic, 38, 5, 245}, want: "\x1b[2;3;38;5;245m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sgr(tc.args...))
		})
	}
}
