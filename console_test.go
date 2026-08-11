package main

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
)

// TestUIConsoleExitSignalsQuit verifies Exit closes the quit channel the
// driver watches.
func TestUIConsoleExitSignalsQuit(t *testing.T) {
	// not parallel: uses a shared channel
	c := &uiConsole{quit: make(chan struct{}, 1)}
	c.Exit()
	select {
	case <-c.quit:
	default:
		t.Fatal("Exit must signal the quit channel")
	}
}

// TestUIConsoleStartedFlag verifies Started reads the shared flag the pump owns.
func TestUIConsoleStartedFlag(t *testing.T) {
	// not parallel: mutates a shared bool
	started := false
	c := &uiConsole{started: &started}
	assert.False(t, c.Started())
	started = true
	assert.True(t, c.Started())
}

// TestUIConsoleToolsChangedNilRecorder verifies ToolsChanged is a no-op when no
// recorder is attached (recording disabled).
func TestUIConsoleToolsChangedNilRecorder(t *testing.T) {
	c := &uiConsole{tools: tools.New()}
	assert.NotPanics(t, func() { c.ToolsChanged() })
}
