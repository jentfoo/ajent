package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// sgr() must join attributes and color params into one escape; cursorTo() is
// exercised by the emulator in vt_test.go.
func TestSgr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []int
		want string
	}{
		{name: "reset", args: nil, want: "\x1b[0m"},
		{name: "attrs_and_color_params", args: []int{attrDim, attrItalic, 38, 5, 245}, want: "\x1b[2;3;38;5;245m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sgr(tc.args...))
		})
	}
}
