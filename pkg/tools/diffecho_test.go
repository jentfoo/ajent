package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiffEcho(t *testing.T) {
	t.Parallel()

	t.Run("parse", func(t *testing.T) {
		tests := []struct {
			name  string
			in    string
			want  DiffEcho
			valid bool
		}{
			{"empty_is_default", "", DiffValidate, true},
			{"validate", "validate", DiffValidate, true},
			{"never", "never", DiffNever, true},
			{"overwrite", "overwrite", DiffOverwrite, true},
			{"always_case_insensitive", "ALWAYS", DiffAlways, true},
			{"unknown_name", "noisy", DiffValidate, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, ok := ParseDiffEcho(tt.in)
				assert.Equal(t, tt.valid, ok)
				assert.Equal(t, tt.want, got)
			})
		}
	})

	// tools.diffEcho persists through these spellings, so String and parse must agree
	t.Run("string_round_trips", func(t *testing.T) {
		for _, m := range []DiffEcho{DiffValidate, DiffNever, DiffOverwrite, DiffAlways} {
			got, ok := ParseDiffEcho(m.String())
			assert.True(t, ok)
			assert.Equal(t, m, got)
		}
		assert.Equal(t, "validate", DiffEcho(99).String()) // unknown falls back to the default
	})
}
