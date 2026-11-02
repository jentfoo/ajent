package tui

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Keymap configures which physical keys submit the prompt and which insert a
// line break. The zero value (empty lists) uses the built-in defaults: Enter
// submits, Ctrl+J and Alt+Enter insert a newline. Build one with
// ParseKeymap to validate names and conflicts. The caller hands it to Options.
type Keymap struct {
	Submit  []string // keys that submit the prompt
	Newline []string // keys that insert a line break
}

// keyRole is the semantic editor action a physical key maps to.
type keyRole uint8

const (
	roleSubmit keyRole = iota
	roleNewline
)

// keymap resolves a physical key name to its role. Only the line-ending keys are
// configurable, every other key keeps its fixed role.
type keymap map[string]keyRole

// Canonical names of the configurable line-ending keys. The decoder tags each
// key it decodes with one of these (key.phys), so defaultKeymap must cover them.
const (
	physNameEnter    = "enter"     // Enter and the keypad enter
	physNameCtrlJ    = "ctrl+j"    // LF, the classic newline control
	physNameAltEnter = "alt+enter" // ESC CR and ESC LF
)

// defaultKeymap holds the built-in bindings, mirroring the historic defaults:
// Enter submits, Ctrl+J and Alt+Enter insert a newline.
var defaultKeymap = keymap{
	physNameEnter:    roleSubmit,
	physNameCtrlJ:    roleNewline,
	physNameAltEnter: roleNewline,
}

// resolve builds the effective keymap from the lists, layering overrides on the
// hard-coded defaults. The config lists are overrides only, never a complete
// binding, so an unlisted key keeps its default role. An unknown name or one
// bound to both roles is an error (a typo or conflict must not fall back), and
// at least one submit key has to survive the overrides.
func (k Keymap) resolve() (keymap, error) {
	km := make(keymap, len(defaultKeymap))
	for name, role := range defaultKeymap {
		km[name] = role
	}
	// explicitSubmit tracks keys the user bound to submit, so a key cannot also
	// be bound to newline. A default submit (e.g. enter) stays overridable to
	// newline: only an explicit pair is a conflict.
	explicitSubmit := make(map[string]bool, len(k.Submit))
	for _, name := range k.Submit {
		if _, ok := defaultKeymap[name]; !ok {
			return nil, fmt.Errorf("unknown submit key %q", name)
		}
		explicitSubmit[name] = true
		km[name] = roleSubmit
	}
	for _, name := range k.Newline {
		if _, ok := defaultKeymap[name]; !ok {
			return nil, fmt.Errorf("unknown newline key %q", name)
		}
		if explicitSubmit[name] {
			return nil, fmt.Errorf("key %q bound to both submit and newline", name)
		}
		km[name] = roleNewline
	}
	// an override stripping every submit binding leaves nothing to send a prompt
	// with, so it falls back like any other invalid keymap
	if !slices.Contains(slices.Collect(maps.Values(km)), roleSubmit) {
		return nil, errors.New("keymap leaves no submit key")
	}
	return km, nil
}

// ParseKeymap validates the submit and newline lists, returning the Keymap
// unchanged on success or an error naming the offending key. A zero Keymap is
// the default and never errors.
func ParseKeymap(submit, newline []string) (Keymap, error) {
	k := Keymap{Submit: submit, Newline: newline}
	_, err := k.resolve()
	return k, err
}

// resolveKeyRole resolves a configurable line-ending key to its semantic action.
// Keys without a configurable physical name (a synthetic key from a test, or a
// key the keymap does not name) keep the role their decoded type implies.
func resolveKeyRole(km keymap, k key) keyRole {
	if k.phys != "" {
		if role, ok := km[k.phys]; ok {
			return role
		}
	}
	if k.typ == keyEnter {
		return roleSubmit
	}
	return roleNewline
}
