package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSchemaDefect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema string
		want   string // "" means sound; otherwise a substring of the defect
	}{
		{name: "bare_object", schema: `{"type":"object"}`, want: ""},
		{
			name:   "absent_properties_accepted",
			schema: `{"type":"object","additionalProperties":false}`,
			want:   "",
		},
		{
			name:   "properties_and_required",
			schema: `{"type":"object","properties":{"path":{"type":"string"},"depth":{"type":"integer"}},"required":["path"]}`,
			want:   "",
		},
		{
			name:   "empty_properties",
			schema: `{"type":"object","properties":{},"required":[]}`,
			want:   "",
		},
		{
			name:   "nested_array_of_objects",
			schema: `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string","enum":["a","b"]}},"required":["name"]}}},"required":["rows"]}`,
			want:   "",
		},
		{
			name:   "type_array_accepted",
			schema: `{"type":"object","properties":{"x":{"type":["string","null"]},"y":{"type":"boolean"}}}`,
			want:   "",
		},
		{
			name:   "unknown_keywords_pass_through",
			schema: `{"type":"object","properties":{"x":{"type":"string","format":"date-time","minimum":3},"y":{"type":"array","items":{"type":"number","exclusiveMinimum":0}}}}`,
			want:   "",
		},
		{
			name:   "untyped_property_accepted",
			schema: `{"type":"object","properties":{"x":{},"y":{"description":"free-form"}}}`,
			want:   "",
		},
		{
			name:   "boolean_schemas_accepted",
			schema: `{"type":"object","properties":{"x":true,"rows":{"type":"array","items":false}}}`,
			want:   "",
		},
		{
			name:   "enum_only_property_accepted",
			schema: `{"type":"object","properties":{"x":{"enum":["a","b"]}}}`,
			want:   "",
		},
		{
			name:   "ref_only_property_accepted",
			schema: `{"type":"object","properties":{"x":{"$ref":"#/$defs/thing"}},"$defs":{"thing":{"type":"string"}}}`,
			want:   "",
		},
		{
			name:   "pattern_properties_shaped_node",
			schema: `{"type":"object","patternProperties":{"^x-":{"type":"string"}},"additionalProperties":{"type":"integer"}}`,
			want:   "",
		},
		{
			name:   "tuple_items_accepted",
			schema: `{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"},{"type":"integer"}]}}}`,
			want:   "",
		},
		{
			name:   "combinator_replaces_type",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"type":"string"},{"type":"null"}]}}}`,
			want:   "",
		},
		{
			name:   "one_of_and_all_of",
			schema: `{"type":"object","properties":{"x":{"oneOf":[{"type":"string"},{"type":"integer"}]},"y":{"allOf":[{"type":"string","minLength":1}]}}}`,
			want:   "",
		},
		{
			name:   "combinator_alongside_type",
			schema: `{"type":"object","properties":{"x":{"type":"string","anyOf":[{"type":"null"}]}}}`,
			want:   "",
		},
		{
			name:   "nested_combinator_branches",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"anyOf":[{"type":"string"}]},{"type":"null"}]}}}`,
			want:   "",
		},
		{
			name:   "constraint_only_branch",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"enum":["a","b"]},{"type":"string"}]}}}`,
			want:   "",
		},
		{
			name:   "ref_branch_accepted",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"$ref":"#/definitions/x"},{"type":"string"}]}}}`,
			want:   "",
		},
		{
			name:   "combinator_items",
			schema: `{"type":"object","properties":{"rows":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}`,
			want:   "",
		},
		{
			name:   "conditional_and_coverage_keywords",
			schema: `{"type":"object","properties":{},"if":{"type":"object"},"then":{"type":"object"},"else":{"type":"object"},"contains":{"type":"string"},"propertyNames":{"type":"string"},"not":{"type":"integer"},"dependentSchemas":{"a":{"type":"object"}},"definitions":{"d":{"type":"string"}}}`,
			want:   "",
		},
		{
			name:   "root_combinator_walked",
			schema: `{"type":"object","allOf":[{"type":"object","properties":{"x":{"type":"nuclear"}}}]}`,
			want:   `allOf[0].properties.x has unknown type "nuclear"`,
		},
		{
			name:   "root_combinator_empty_accepted",
			schema: `{"type":"object","anyOf":[]}`,
			want:   "",
		},
		{
			name:   "root_combinator_not_an_array",
			schema: `{"type":"object","allOf":"string"}`,
			want:   "allOf is not an array",
		},
		{
			name:   "root_ref_and_enum_checked",
			schema: `{"type":"object","$ref":5}`,
			want:   ".$ref is not a non-empty string",
		},
		{
			name:   "root_enum_not_an_array",
			schema: `{"type":"object","enum":"a"}`,
			want:   ".enum is not an array",
		},
		{
			name:   "unreferenced_defs_not_walked",
			schema: `{"type":"object","properties":{"a":{"type":"string"}},"$defs":{"legacy":{"type":"file"}},"definitions":{"old":{"type":"any"}}}`,
			want:   "",
		},
		{
			name:   "draft03_boolean_required_accepted",
			schema: `{"type":"object","properties":{"q":{"type":"string","required":true}}}`,
			want:   "",
		},
		{
			name:   "property_key_dotted_accepted",
			schema: `{"type":"object","properties":{"filter.status":{"type":"string"}}}`,
			want:   "",
		},
		{
			name:   "property_key_64_chars_accepted",
			schema: `{"type":"object","properties":{"` + strings.Repeat("x", 64) + `":{"type":"string"}}}`,
			want:   "",
		},
		{
			name:   "property_key_too_long_rejected",
			schema: `{"type":"object","properties":{"` + strings.Repeat("x", 65) + `":{"type":"string"}}}`,
			want:   "is not a valid property name",
		},
		{
			// Han key via JSON \u escape, matching what a real server sends
			name:   "property_key_non_ascii_rejected",
			schema: `{"type":"object","properties":{"\u57ce\u5e02":{"type":"string"}}}`,
			want:   "is not a valid property name",
		},
		{
			name:   "pattern_properties_key_is_a_regex",
			schema: `{"type":"object","patternProperties":{"^x-.$":{"type":"string"}}}`,
			want:   "",
		},
		{
			name:   "top_not_an_object",
			schema: `{"type":"string"}`,
			want:   `type must be "object"`,
		},
		{name: "top_type_missing", schema: `{"properties":{}}`, want: `type must be "object"`},
		{name: "top_type_array", schema: `{"type":["object"]}`, want: `type must be "object"`},
		{name: "top_not_json", schema: `[]`, want: "not a JSON object"},
		{name: "top_truncated_json", schema: `{"type":`, want: "not a JSON object"},
		{
			name:   "properties_not_an_object",
			schema: `{"type":"object","properties":"x"}`,
			want:   "properties is not an object",
		},
		{
			name:   "property_value_not_an_object",
			schema: `{"type":"object","properties":{"x":"string"}}`,
			want:   "properties.x is not an object",
		},
		{
			name:   "property_value_array",
			schema: `{"type":"object","properties":{"x":[]}}`,
			want:   "properties.x is not an object",
		},
		{
			name:   "property_type_unknown_name",
			schema: `{"type":"object","properties":{"x":{"type":"strng"}}}`,
			want:   `properties.x has unknown type "strng"`,
		},
		{
			name:   "property_type_wrong_kind",
			schema: `{"type":"object","properties":{"x":{"type":42}}}`,
			want:   "properties.x has a malformed type",
		},
		{
			name:   "property_type_empty_array",
			schema: `{"type":"object","properties":{"x":{"type":[]}}}`,
			want:   "properties.x has a malformed type",
		},
		{
			name:   "nested_property_rejected",
			schema: `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"k"}}}}}}}`,
			want:   `properties.a.properties.b.properties.c has unknown type "k"`,
		},
		{
			name:   "items_not_an_object",
			schema: `{"type":"object","properties":{"rows":{"type":"array","items":"string"}}}`,
			want:   "properties.rows.items is not an object",
		},
		{
			name:   "tuple_item_rejected",
			schema: `{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"},{"type":"k"}]}}}`,
			want:   `properties.pair.items[1] has unknown type "k"`,
		},
		{
			name:   "combinator_not_an_array",
			schema: `{"type":"object","properties":{"x":{"anyOf":"string"}}}`,
			want:   "properties.x.anyOf is not an array",
		},
		{
			name:   "combinator_empty_accepted",
			schema: `{"type":"object","properties":{"x":{"anyOf":[]}}}`,
			want:   "",
		},
		{
			name:   "enum_empty_accepted",
			schema: `{"type":"object","properties":{"x":{"enum":[]}}}`,
			want:   "",
		},
		{
			name:   "null_keywords_tolerated",
			schema: `{"type":"object","properties":null,"required":null,"additionalProperties":null,"anyOf":null,"enum":null,"$ref":null}`,
			want:   "",
		},
		{
			name:   "null_nested_keywords_tolerated",
			schema: `{"type":"object","properties":{"x":{"type":null,"items":null,"anyOf":null,"enum":null}}}`,
			want:   "",
		},
		{
			name:   "combinator_branch_not_an_object",
			schema: `{"type":"object","properties":{"x":{"anyOf":["string"]}}}`,
			want:   "properties.x.anyOf[0] is not an object",
		},
		{
			name:   "combinator_branch_bad_type",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"type":"strng"}]}}}`,
			want:   `properties.x.anyOf[0] has unknown type "strng"`,
		},
		{
			name:   "ref_not_a_string",
			schema: `{"type":"object","properties":{"x":{"$ref":5}}}`,
			want:   ".$ref is not a non-empty string",
		},
		{
			name:   "enum_not_an_array",
			schema: `{"type":"object","properties":{"x":{"enum":"a"}}}`,
			want:   ".enum is not an array",
		},
		{
			name:   "additional_properties_bad_type",
			schema: `{"type":"object","additionalProperties":{"type":"nope"}}`,
			want:   `additionalProperties has unknown type "nope"`,
		},
		{
			name:   "pattern_properties_not_an_object",
			schema: `{"type":"object","patternProperties":[]}`,
			want:   "patternProperties is not an object",
		},
		{
			name:   "required_not_strings",
			schema: `{"type":"object","properties":{"x":{"type":"string"}},"required":["x",1]}`,
			want:   "required is not an array of property names",
		},
		{
			name:   "required_not_an_array",
			schema: `{"type":"object","required":"x"}`,
			want:   "required is not an array of property names",
		},
		{
			name:   "required_names_unknown_property_accepted",
			schema: `{"type":"object","properties":{"x":{"type":"string"}},"required":["y"]}`,
			want:   "",
		},
		{
			name:   "required_without_properties_accepted",
			schema: `{"type":"object","required":["y"]}`,
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaDefect(json.RawMessage(tt.schema))
			if tt.want == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tt.want)
			}
		})
	}
}

func TestSchemaDefectDepthCap(t *testing.T) {
	t.Parallel()

	nested := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"type":"object","properties":{"x":`)
		for range n - 1 {
			b.WriteString(`{"type":"object","properties":{"x":`)
		}
		b.WriteString(`{"type":"string"}`)
		for range n - 1 {
			b.WriteString(`}}`)
		}
		b.WriteString(`}}`)
		return b.String()
	}

	t.Run("shallow_accepted", func(t *testing.T) {
		assert.Empty(t, schemaDefect(json.RawMessage(nested(maxSchemaDepth))))
	})
	t.Run("too_deep_rejected", func(t *testing.T) {
		got := schemaDefect(json.RawMessage(nested(maxSchemaDepth + 1)))
		assert.Contains(t, got, "nested too deeply")
	})
	t.Run("deep_bytes_stay_fast", func(t *testing.T) {
		// a single decode bounds work: a 2000-deep schema answers without the
		// quadratic re-decode the raw walker used to pay per level
		raw := json.RawMessage(nested(2000))
		assert.Contains(t, schemaDefect(raw), "nested too deeply")
	})
}
