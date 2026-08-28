package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		data  string // empty starts from nothing
		key   string
		value any
		want  string
	}{
		{
			"sets_nested_value_preserving_unknowns",
			`{"model":"m","customThing":{"z":1,"a":2},"reasoning":{"level":"low"}}`,
			"reasoning.level", "high",
			`{"model":"m","customThing":{"z":1,"a":2},"reasoning":{"level":"high"}}`,
		},
		{
			"creates_nested_objects_from_empty",
			"", "tools.limits.bash.bytes", 4096,
			`{"tools":{"limits":{"bash":{"bytes":4096}}}}`,
		},
		{
			"replaces_non_object_intermediate",
			`{"a":5}`, "a.b", true,
			`{"a":{"b":true}}`,
		},
		{
			"sets_array_value",
			"", "tools.enabled", []string{"read", "bash"},
			`{"tools":{"enabled":["read","bash"]}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := SetKey([]byte(tc.data), tc.key, tc.value)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(out))
		})
	}

	t.Run("preserves_key_order", func(t *testing.T) {
		// a new key is appended after existing ones; unknown keys keep their place
		out, err := SetKey([]byte(`{"z":1,"a":{"b":2}}`), "m", 3)
		require.NoError(t, err)
		assert.Less(t, strings.Index(string(out), `"z"`), strings.Index(string(out), `"m"`))
	})
	t.Run("unserializable_value_errors", func(t *testing.T) {
		// a value json.Marshal rejects surfaces as an error rather than corrupting the layer
		_, err := SetKey([]byte(`{}`), "a", make(chan int))
		assert.Error(t, err)
	})
}
