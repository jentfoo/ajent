package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jentfoo/ajent/pkg/tui"
)

func TestTuiOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels []string
		want   []tui.Option
	}{
		{"empty", nil, []tui.Option{}},
		{"single", []string{"one"}, []tui.Option{{Label: "one"}}},
		{"multiple", []string{"one", "two"}, []tui.Option{{Label: "one"}, {Label: "two"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tuiOptions(tt.labels))
		})
	}
}
