package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseKeymap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		submit  []string
		newline []string
		want    keymap
		err     string
	}{
		{
			name:    "zero_value_defaults",
			submit:  nil,
			newline: nil,
			want:    defaultKeymap,
		},
		{
			name:    "override_enter_to_newline",
			submit:  []string{"alt+enter"},
			newline: []string{"enter"},
			want: keymap{
				physNameEnter:    roleNewline,
				physNameCtrlJ:    roleNewline,
				physNameAltEnter: roleSubmit,
			},
		},
		{
			name:    "override_ctrl_j_to_submit",
			submit:  []string{"ctrl+j"},
			newline: nil,
			want: keymap{
				physNameEnter:    roleSubmit,
				physNameCtrlJ:    roleSubmit,
				physNameAltEnter: roleNewline,
			},
		},
		{
			name:    "redundant_binding_ok",
			submit:  nil,
			newline: []string{"ctrl+j"},
			want: keymap{
				physNameEnter:    roleSubmit,
				physNameCtrlJ:    roleNewline,
				physNameAltEnter: roleNewline,
			},
		},
		{
			name:    "newline_only_no_submit",
			submit:  nil,
			newline: []string{"enter"},
			err:     `keymap leaves no submit key`,
		},
		{
			name:    "unknown_submit_key",
			submit:  []string{"entr"},
			newline: nil,
			err:     `unknown submit key "entr"`,
		},
		{
			name:    "unknown_newline_key",
			submit:  nil,
			newline: []string{"enter!"},
			err:     `unknown newline key "enter!"`,
		},
		{
			name:    "bound_to_both",
			submit:  []string{"enter"},
			newline: []string{"enter"},
			err:     `key "enter" bound to both submit and newline`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseKeymap(tc.submit, tc.newline)
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err)
			km, err := k.resolve()
			require.NoError(t, err)
			assert.Equal(t, tc.want, km)
		})
	}
}

// TestDefaultKeymapCoversPhysNames pins every canonical phys name to a binding,
// so a decoder tag can never outlive its entry in defaultKeymap.
func TestDefaultKeymapCoversPhysNames(t *testing.T) {
	t.Parallel()

	km, err := Keymap{}.resolve()
	require.NoError(t, err)
	for _, name := range []string{physNameEnter, physNameCtrlJ, physNameAltEnter} {
		assert.Contains(t, km, name)
	}
}

// keymapUI builds a test UI with the given keymap and a helper that applies a
// key and returns the submitted line (nil when the key only edited).
func keymapUI(t *testing.T, submit, newline []string) (*UI, func(key) *string, func() string) {
	t.Helper()

	u := newTestUI(t, newVT(40, 10), strings.NewReader(""))
	km, err := ParseKeymap(submit, newline)
	require.NoError(t, err)
	resolved, err := km.resolve()
	require.NoError(t, err)
	u.mu.Lock()
	u.keymap = resolved
	u.mu.Unlock()

	apply := func(k key) *string {
		u.mu.Lock()
		defer u.mu.Unlock()
		s, _, _ := u.applyKey(k)
		return s
	}
	value := func() string {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.editor.Value()
	}
	return u, apply, value
}

func TestUIKeymapRemapped(t *testing.T) {
	t.Parallel()

	_, apply, value := keymapUI(t, []string{"alt+enter"}, []string{"enter"})

	// a physical Enter is remapped to newline: it edits, it does not submit
	apply(key{typ: keyRune, text: "h"})
	apply(key{typ: keyRune, text: "i"})
	apply(key{typ: keyEnter, phys: "enter"})
	assert.Equal(t, "hi\n", value())

	// Alt+Enter submits what was composed
	s := apply(key{typ: keyNewline, phys: "alt+enter"})
	require.NotNil(t, s)
	assert.Equal(t, "hi\n", *s)
}

func TestUIKeymapDefault(t *testing.T) {
	t.Parallel()

	_, apply, value := keymapUI(t, nil, nil)

	// default keymap: Enter submits, Ctrl+J inserts a newline
	apply(key{typ: keyRune, text: "a"})
	apply(key{typ: keyRune, text: "b"})
	apply(key{typ: keyNewline, phys: "ctrl+j"})
	assert.Equal(t, "ab\n", value())

	s := apply(key{typ: keyEnter, phys: "enter"})
	require.NotNil(t, s)
	assert.Equal(t, "ab\n", *s)
}

func TestKeyRoleFallback(t *testing.T) {
	t.Parallel()

	u := &UI{keymap: defaultKeymap}
	// a configurable key resolves through the map
	assert.Equal(t, roleSubmit, u.keyRole(key{typ: keyEnter, phys: "enter"}))
	assert.Equal(t, roleNewline, u.keyRole(key{typ: keyNewline, phys: "ctrl+j"}))
	// a key the map does not name falls back to its decoded type
	assert.Equal(t, roleSubmit, u.keyRole(key{typ: keyEnter}))
	assert.Equal(t, roleNewline, u.keyRole(key{typ: keyNewline}))
}
