//go:build !demo

package main

import (
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// drive starts the real agent loop. The `demo` build tag swaps in demo.go's
// scripted stand-in instead.
func drive(ui *tui.UI, set *config.Set, reg *llm.Registry, active llm.Model, sessMode resumeMode, resumeID string, args []string) *sessRec {
	return driver(ui, set, reg, active, sessMode, resumeID, args)
}
