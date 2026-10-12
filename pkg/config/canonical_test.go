package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalizeKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		renames []keyRename
	}{
		{
			name: "nested_rename",
			in:   `{"UI":{"Theme":"midnight"}}`,
			want: `{"ui":{"theme":"midnight"}}`,
			renames: []keyRename{
				{from: "UI", to: "ui"},
				{from: "UI.Theme", to: "ui.theme"},
			},
		},
		{
			name:    "unknown_keys_untouched",
			in:      `{"bogusKey":{"Weird":1},"MODEL":"m"}`,
			want:    `{"bogusKey":{"Weird":1},"model":"m"}`,
			renames: []keyRename{{from: "MODEL", to: "model"}},
		},
		{
			name:    "case_twins_last_value_wins",
			in:      `{"MODEL":"a","model":"b"}`,
			want:    `{"model":"b"}`,
			renames: []keyRename{{from: "MODEL", to: "model"}},
		},
		{
			name:    "canonical_first_odd_second",
			in:      `{"model":"a","MODEL":"b"}`,
			want:    `{"model":"b"}`,
			renames: []keyRename{{from: "MODEL", to: "model"}},
		},
		{
			name: "array_values_traversed",
			in:   `{"PERMISSIONS":{"SAFECOMMANDS":["git status"],"MODE":"allow-read"}}`,
			want: `{"permissions":{"safeCommands":["git status"],"mode":"allow-read"}}`,
			renames: []keyRename{
				{from: "PERMISSIONS", to: "permissions"},
				{from: "PERMISSIONS.SAFECOMMANDS", to: "permissions.safeCommands"},
				{from: "PERMISSIONS.MODE", to: "permissions.mode"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, renames := canonicalizeKeys([]byte(tc.in))
			assert.JSONEq(t, tc.want, string(out))
			assert.Equal(t, tc.renames, renames)
		})
	}

	t.Run("already_canonical_byte_identical", func(t *testing.T) {
		data := Defaults().Data
		out, renames := canonicalizeKeys(data)
		assert.Empty(t, renames)
		assert.Equal(t, string(data), string(out))
	})

	t.Run("malformed_input_unchanged", func(t *testing.T) {
		out, renames := canonicalizeKeys([]byte(`{`))
		assert.Empty(t, renames)
		assert.Equal(t, `{`, string(out))
	})
}
