package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testScalars struct {
	F      float64 `json:"f"`
	Quoted int     `json:"quoted,string"`
	Str    string  `json:"str,string"`
}

func TestTypeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     string
		target   any
		expected []string
	}{
		{"all_fit", `{"name":"a","inner":{"x":1,"y":"b"},"ptrIn":{"x":2},"byKey":{"k":{"x":3}},"list":[{"x":4}],"raw":[1]}`, testOuter{}, nil},
		{"string_for_int", `{"inner":{"x":"5"}}`, testOuter{},
			[]string{`wrong type for key "inner.x": got a string, want an integer`}},
		{"bool_for_string", `{"name":true}`, testOuter{},
			[]string{`wrong type for key "name": got a boolean, want a string`}},
		{"number_for_string", `{"name":123}`, testOuter{},
			[]string{`wrong type for key "name": got a number, want a string`}},
		{"float_for_int", `{"inner":{"x":1.5}}`, testOuter{},
			[]string{`wrong type for key "inner.x": got a number, want an integer`}},
		{"int_overflow", `{"inner":{"x":99999999999999999999}}`, testOuter{},
			[]string{`wrong type for key "inner.x": got a number, want an integer`}},
		{"object_for_scalar", `{"name":{"a":1}}`, testOuter{},
			[]string{`wrong type for key "name": got an object, want a string`}},
		{"array_for_object", `{"ptrIn":[1]}`, testOuter{},
			[]string{`wrong type for key "ptrIn": got an array, want an object`}},
		{"scalar_element_for_struct", `{"list":[1]}`, testOuter{},
			[]string{`wrong type for key "list[]": got a number, want an object`}},
		{"map_value_type_error", `{"byKey":{"k":"x"}}`, testOuter{},
			[]string{`wrong type for key "byKey.k": got a string, want an object`}},
		{"null_fits_anything", `{"name":null,"inner":null,"raw":null}`, testOuter{}, nil},
		{"raw_message_opaque", `{"raw":{"anything":[1,{"x":true}]}}`, testOuter{}, nil},
		{"unknown_key_skipped", `{"inner":{"zz":"bad"}}`, testOuter{}, nil},
		{"case_insensitive_path", `{"INNER":{"X":"bad"}}`, testOuter{},
			[]string{`wrong type for key "INNER.X": got a string, want an integer`}},
		{"multiple_reported", `{"name":1,"inner":{"x":"bad"}}`, testOuter{},
			[]string{
				`wrong type for key "name": got a number, want a string`,
				`wrong type for key "inner.x": got a string, want an integer`,
			}},

		{"int_literal_for_float", `{"f":2}`, testScalars{}, nil},
		{"string_for_float", `{"f":"x"}`, testScalars{},
			[]string{`wrong type for key "f": got a string, want a number`}},
		{"bool_for_float", `{"f":false}`, testScalars{},
			[]string{`wrong type for key "f": got a boolean, want a number`}},
		{"quoted_number_ok", `{"quoted":"7"}`, testScalars{}, nil},
		{"unquoted_rejected", `{"quoted":7}`, testScalars{},
			[]string{`wrong type for key "quoted": got a number, want a quoted integer`}},
		{"bad_quoted_content", `{"quoted":"abc"}`, testScalars{},
			[]string{`wrong type for key "quoted": got a string, want a quoted integer`}},
		{"fractional_quoted_rejected", `{"quoted":"1.5"}`, testScalars{},
			[]string{`wrong type for key "quoted": got a string, want a quoted integer`}},
		{"quoted_null_ok", `{"quoted":"null","str":"null"}`, testScalars{}, nil},
		{"doubly_quoted_string_ok", `{"str":"\"hi\""}`, testScalars{}, nil},
		{"plain_string_rejected", `{"str":"hi"}`, testScalars{},
			[]string{`wrong type for key "str": got a string, want a quoted string`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TypeErrors([]byte(tc.data), tc.target)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.expected, got)
		})
	}

	t.Run("malformed_json_errors", func(t *testing.T) {
		_, err := TypeErrors([]byte(`{`), testOuter{})
		assert.Error(t, err)
	})
}
