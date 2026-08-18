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

func TestCursorUp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{name: "zero_is_noop", n: 0},
		{name: "negative_is_noop", n: -1},
		{name: "moves_up_n_rows", n: 3, want: "\x1b[3A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cursorUp(tc.n))
		})
	}
}

func TestCursorRight(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{name: "zero_is_noop", n: 0},
		{name: "moves_right_n_cols", n: 5, want: "\x1b[5C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cursorRight(tc.n))
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
