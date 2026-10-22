package app

import (
	"github.com/jentfoo/ajent/pkg/tui"
)

// tuiOptions labels each string as one select option, in order.
func tuiOptions(labels []string) []tui.Option {
	opts := make([]tui.Option, len(labels))
	for i, o := range labels {
		opts[i] = tui.Option{Label: o}
	}
	return opts
}
