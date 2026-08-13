package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetKeyPreservesUnknownKeysAndOrder(t *testing.T) {
	t.Parallel()

	data := []byte(`{"model":"m","customThing":{"z":1,"a":2},"reasoning":{"level":"low"}}`)
	out, err := SetKey(data, "reasoning.level", "high")
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, "high", m["reasoning"].(map[string]any)["level"])
	// unknown keys survive and keep their order relative to the rest
	_, ok := m["customThing"]
	require.True(t, ok)
}

func TestSetKeyCreatesNestedObjects(t *testing.T) {
	t.Parallel()

	out, err := SetKey(nil, "tools.limits.bash.bytes", 4096)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.InDelta(t, float64(4096), m["tools"].(map[string]any)["limits"].(map[string]any)["bash"].(map[string]any)["bytes"], 0)
}

func TestSetKeyReplacesNonObjectIntermediate(t *testing.T) {
	t.Parallel()

	out, err := SetKey([]byte(`{"a":5}`), "a.b", true)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, true, m["a"].(map[string]any)["b"])
}

func TestSetKeyArrayValue(t *testing.T) {
	t.Parallel()

	out, err := SetKey(nil, "tools.enabled", []string{"read", "bash"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	enabled := m["tools"].(map[string]any)["enabled"].([]any)
	assert.Equal(t, []any{"read", "bash"}, enabled)
}

func TestGetKeyRoundTrip(t *testing.T) {
	t.Parallel()

	data := []byte(`{"reasoning":{"level":"low"},"tools":{"enabled":["a","b"]}}`)
	v, ok := GetKey(data, "reasoning.level")
	require.True(t, ok)
	assert.Equal(t, `"low"`, string(v))

	arr, ok := GetKey(data, "tools.enabled")
	require.True(t, ok)
	assert.Contains(t, string(arr), "a")

	_, ok = GetKey(data, "nope.deep")
	assert.False(t, ok)
}
