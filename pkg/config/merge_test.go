package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeObjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		layers   []string
		expected string
	}{
		{"no_layers", nil, `{}`},
		{
			"later_scalar_wins",
			[]string{`{"a":1,"b":2}`, `{"b":3}`},
			`{"a":1,"b":3}`,
		},
		{
			"nested_objects_merge",
			[]string{`{"p":{"x":1,"y":2}}`, `{"p":{"y":9,"z":3}}`},
			`{"p":{"x":1,"y":9,"z":3}}`,
		},
		{
			"arrays_replaced_not_appended",
			[]string{`{"a":[1,2,3]}`, `{"a":[9]}`},
			`{"a":[9]}`,
		},
		{
			"object_replaces_scalar",
			[]string{`{"a":1}`, `{"a":{"b":2}}`},
			`{"a":{"b":2}}`,
		},
		{
			"scalar_replaces_object",
			[]string{`{"a":{"b":2}}`, `{"a":1}`},
			`{"a":1}`,
		},
		{
			"blank_layers_skipped",
			[]string{``, `{"a":1}`, `   `},
			`{"a":1}`,
		},
		{
			"three_layers",
			[]string{`{"a":1}`, `{"b":2}`, `{"a":3,"c":4}`},
			`{"a":3,"b":2,"c":4}`,
		},
		{
			"large_int_preserved",
			[]string{`{"n":9007199254740993}`},
			`{"n":9007199254740993}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layers := make([][]byte, len(tc.layers))
			for i, l := range tc.layers {
				layers[i] = []byte(l)
			}
			got, err := MergeObjects(layers...)
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(got))
		})
	}

	t.Run("malformed_layer_errors", func(t *testing.T) {
		_, err := MergeObjects([]byte(`{"a":1}`), []byte(`{`))
		assert.Error(t, err)
	})
	t.Run("source_layer_not_mutated", func(t *testing.T) {
		base := []byte(`{"p":{"x":1}}`)
		_, err := MergeObjects(base, []byte(`{"p":{"y":2}}`))
		require.NoError(t, err)
		assert.JSONEq(t, `{"p":{"x":1}}`, string(base))
	})
}
