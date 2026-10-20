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
		// both type spellings take the object closure passes
		assert.Equal(t, []any{}, prop["required"])
		assert.Equal(t, false, prop["additionalProperties"])
	})

	t.Run("object_type_array_with_properties_closed", func(t *testing.T) {
		// the type-array spelling of an object gets the same closure as the
		// string spelling, so strictness does not depend on authoring style
		in := rewrittenOK(t, `{"type":"object","properties":{"cfg":`+
			`{"type":["object","null"],"properties":{"x":{"type":"string"}},"required":["x"]}},`+
			`"required":["cfg"]}`)
		cfg := in["properties"].(map[string]any)["cfg"].(map[string]any)
		assert.Equal(t, []any{"x"}, cfg["required"])
		assert.Equal(t, false, cfg["additionalProperties"])
		assert.NotContains(t, cfg["properties"].(map[string]any)["x"], "anyOf")
	})

	t.Run("optional_structured_property_refused", func(t *testing.T) {
		// null-wrapping an optional array or object builds exactly the
		// structured-member anyOf the provider refuses, so the tool falls
		// back to shipping non-strict
		for _, raw := range []string{
			`{"type":"object","properties":{"rows":` +
				`{"type":"array","items":{"type":"string"}}}}`,
			`{"type":"object","properties":{"cfg":` +
				`{"type":"object","properties":{"x":{"type":"string"}}}}}`,
			`{"type":"object","properties":{"cfg":{"type":"object"}}}`,
		} {
			_, ok := makeStrictAnthropicSchema(json.RawMessage(raw))
			assert.False(t, ok, raw)
		}
	})

	t.Run("optional_nullable_structured_property_strict", func(t *testing.T) {
		// the nullable type-array spelling of the same optional stays strict
		in := rewrittenOK(t, `{"type":"object","properties":{"rows":`+
			`{"type":["array","null"],"items":{"type":"string"}}}}`)
		rows := in["properties"].(map[string]any)["rows"].(map[string]any)
		assert.Equal(t, []any{"array", "null"}, rows["type"])
		assert.NotContains(t, rows, "anyOf")
		assert.Equal(t, []any{"rows"}, in["required"])
	})

	t.Run("optional_nullable_type_array_not_rewrapped", func(t *testing.T) {
		// the optional spelling reaches the type-array null check; a rewrap
		// would double-count the union budget
		in := rewrittenOK(t, `{"type":"object","properties":`+
			`{"a":{"type":["string","null"]}}}`)
		prop := in["properties"].(map[string]any)["a"].(map[string]any)
		assert.Equal(t, []any{"string", "null"}, prop["type"])
		assert.NotContains(t, prop, "anyOf")
		assert.Equal(t, []any{"a"}, in["required"])
	})

	t.Run("null_keywords_treated_as_absent", func(t *testing.T) {
		// the MCP layer tolerates explicit nulls where a keyword is optional;
		// the rewrite must not ship them nor lose strictness for them
		in := rewrittenOK(t, `{"type":"object","properties":null,`+
			`"required":null,"additionalProperties":null,"anyOf":null,`+
			`"enum":null,"$ref":null}`)
		assert.Equal(t, []any{}, in["required"])
		assert.Equal(t, false, in["additionalProperties"])
		for _, key := range []string{"properties", "anyOf", "enum", "$ref"} {
			assert.NotContains(t, in, key)
		}
	})

	t.Run("null_nested_keywords_treated_absent", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object","properties":{"x":`+
			`{"type":"string","items":null,"anyOf":null,"enum":null}},`+
			`"required":["x"]}`)
		x := in["properties"].(map[string]any)["x"].(map[string]any)
		assert.Equal(t, map[string]any{"type": "string"}, x)
	})

	t.Run("required_null_all_optional", func(t *testing.T) {
		in := rewrittenOK(t, `{"type":"object","properties":`+
			`{"a":{"type":"string"},"b":{"type":["integer","null"]}},`+
			`"required":null}`)
		props := in["properties"].(map[string]any)
		assert.ElementsMatch(t, []any{"a", "b"}, in["required"])
		assert.Len(t, props["a"].(map[string]any)["anyOf"].([]any), 2)
		assert.NotContains(t, props["b"], "anyOf")
	})

	t.Run("required_unknown_name_refused", func(t *testing.T) {
		// strict requires every listed name to exist as a property
		_, ok := makeStrictAnthropicSchema(json.RawMessage(`{"type":"object",` +
			`"properties":{"a":{"type":"string"}},"required":["b"]}`))
		assert.False(t, ok)
	})

	t.Run("required_bool_form_refused", func(t *testing.T) {
		// a boolean required spelling is tolerated upstream but carries no
		// list to make total, so the tool ships non-strict
		_, ok := makeStrictAnthropicSchema(json.RawMessage(`{"type":"object",` +
			`"properties":{"a":{"type":"string"}},"required":true}`))
		assert.False(t, ok)
	})
}
