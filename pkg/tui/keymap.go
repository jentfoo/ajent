package tui

import "fmt"

// Keymap configures which physical keys submit the prompt and which insert a
// line break. The zero value (empty lists) uses the built-in defaults: Enter
// submits, Ctrl+J and Alt+Enter insert a newline. Build one with
// ParseKeymap to validate names and conflicts; the caller hands it to Options.
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
// configurable; every other key keeps its fixed role.
type keymap map[string]keyRole

// defaultKeymap holds the built-in bindings, mirroring the historic defaults:
// Enter submits, Ctrl+J and Alt+Enter insert a newline.
var defaultKeymap = keymap{
	"enter":     roleSubmit,
	"ctrl+j":    roleNewline,
	"alt+enter": roleNewline,
}

// resolve builds the effective keymap from the lists, layering overrides on the
// hard-coded defaults. The config lists are overrides only, never a complete
// binding, so a partial keymap (e.g. just `newline`) leaves the unlisted keys at
// their defaults. An unknown name or a name the user bound to both roles is an
// error so a typo or a conflicting pair cannot silently fall back.
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
