package tui

import "errors"

var (
	// ErrCancelled is returned when the user dismissed an interaction, or the
	// UI closed while one was pending.
	ErrCancelled = errors.New("tui: cancelled")
	// ErrNoUI is returned when there is no terminal to ask through, so the
	// caller must decide the policy itself.
	ErrNoUI = errors.New("tui: no interactive terminal")
)
