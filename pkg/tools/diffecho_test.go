package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseDiffEcho(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  DiffEcho
		valid bool
	}{
		{"empty is default", "", DiffValidate, true},
		{"validate", "validate", DiffValidate, true},
		{"never", "never", DiffNever, true},
		{"overwrite", "overwrite", DiffOverwrite, true},
		{"always case insensitive", "ALWAYS", DiffAlways, true},
		{"unknown name", "noisy", DiffValidate, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDiffEcho(tt.in)
			assert.Equal(t, tt.valid, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDiffEchoString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "validate", DiffValidate.String())
	assert.Equal(t, "never", DiffNever.String())
	assert.Equal(t, "overwrite", DiffOverwrite.String())
	assert.Equal(t, "always", DiffAlways.String())
	assert.Equal(t, "validate", DiffEcho(99).String()) // unknown falls back to the default
}
