package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeStrictAnthropicSchema(t *testing.T) {
	t.Parallel()

	rewrittenOK := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		out, ok := makeStrictAnthropicSchema(json.RawMessage(raw))
		require.True(t, ok)
		var m map[string]any
		require.NoError(t, json.Unmarshal(out, &m))
		return m
	}

	t.Run("simple_object_is_rewritten", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object","properties":{"path":{"type":"string"}}}`)
		assert.Equal(t, []any{"path"}, in["required"])
		assert.Equal(t, false, in["additionalProperties"])
	})

	t.Run("optional_property_null_wrapped", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object",`+
			`"properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`)
		props := in["properties"].(map[string]any)
		assert.ElementsMatch(t, []any{"a", "b"}, in["required"])
		// the optional property becomes a null union so required stays total
		anyOf := props["b"].(map[string]any)["anyOf"].([]any)
		assert.Len(t, anyOf, 2)
	})

	t.Run("union_object_rejected", func(t *testing.T) {
		_, ok := makeStrictAnthropicSchema(json.RawMessage(`{"type":"object",` +
			`"properties":{"x":{"anyOf":[{"type":"string"},{"type":"object"}]}}}`))

		assert.False(t, ok)
	})

	t.Run("non_object_root_rejected", func(t *testing.T) {
		_, ok := makeStrictAnthropicSchema(json.RawMessage(`{"type":"string"}`))
		assert.False(t, ok)
	})

	t.Run("empty_properties_required_array", func(t *testing.T) {
		// the zero-argument tool shape: nil required would marshal as null
		in := rewrittenOK(t, `{"type":"object","properties":{}}`)
		assert.Equal(t, []any{}, in["required"])
		assert.Equal(t, false, in["additionalProperties"])
	})

	t.Run("missing_properties_required_array", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object"}`)
		assert.Equal(t, []any{}, in["required"])
		assert.Equal(t, false, in["additionalProperties"])
	})

	t.Run("nested_propertyless_object_required_array", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object","properties":{"cfg":{"type":"object"}},`+
			`"required":["cfg"]}`)
		cfg := in["properties"].(map[string]any)["cfg"].(map[string]any)
		assert.Equal(t, []any{}, cfg["required"])
		assert.Equal(t, false, cfg["additionalProperties"])
	})

	t.Run("nullable_type_array_not_rewrapped", func(t *testing.T) {
		// already a strict union; a wrap would double-count the union budget
		in := rewrittenOK(t, `{"type":"object","properties":`+
			`{"a":{"type":["string","null"]},"b":{"type":"string"}},"required":["a"]}`)
		props := in["properties"].(map[string]any)
		assert.Equal(t, []any{"string", "null"}, props["a"].(map[string]any)["type"])
		assert.NotContains(t, props["a"], "anyOf")
		// the non-nullable optional property still gets the single null wrap
		assert.Len(t, props["b"].(map[string]any)["anyOf"].([]any), 2)
	})

	t.Run("nullable_structured_type_array_not_wrapped", func(t *testing.T) {
		// a wrap here would put a structured variant inside anyOf, the shape
		// rejected for user-declared unions
		in := rewrittenOK(t, `{"type":"object","properties":`+
			`{"a":{"type":["object","null"]}},"required":["a"]}`)
		prop := in["properties"].(map[string]any)["a"].(map[string]any)
		assert.Equal(t, []any{"object", "null"}, prop["type"])
		assert.NotContains(t, prop, "anyOf")
	})
}
