package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testInner struct {
	X int    `json:"x"`
	Y string `json:"y,omitempty"`
}

type testEmbedded struct {
	Shared string `json:"shared"`
}

type testEmbeddedPtr struct {
	Deep string `json:"deep"`
}

type testOuter struct {
	testEmbedded
	*testEmbeddedPtr
	Name    string               `json:"name"`
	Inner   testInner            `json:"inner"`
	PtrIn   *testInner           `json:"ptrIn"`
	ByKey   map[string]testInner `json:"byKey"`
	List    []testInner          `json:"list"`
	Raw     json.RawMessage      `json:"raw"`
	Skipped string               `json:"-"`
	unused  string               //nolint:unused // exercises the unexported field skip
}

func TestUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     string
		expected []string
	}{
		{"all_known", `{"name":"a","inner":{"x":1,"y":"b"}}`, nil},
		{"top_level_typo", `{"nmme":"a"}`, []string{"nmme"}},
		{"nested_typo", `{"inner":{"z":1}}`, []string{"inner.z"}},
		{"through_pointer", `{"ptrIn":{"z":1}}`, []string{"ptrIn.z"}},
		{"map_keys_allowed", `{"byKey":{"anything":{"x":1}}}`, nil},
		{"map_value_typo", `{"byKey":{"k":{"z":1}}}`, []string{"byKey.k.z"}},
		{"slice_element_typo", `{"list":[{"x":1},{"z":2}]}`, []string{"list[].z"}},
		{"raw_message_opaque", `{"raw":{"anything":{"goes":1}}}`, nil},
		{"json_dash_is_unknown", `{"Skipped":"x"}`, []string{"Skipped"}},
		{"case_insensitive_match", `{"Name":"a","INNER":{"X":1}}`, nil},
		{"multiple_reported", `{"a":1,"inner":{"b":2}}`, []string{"a", "inner.b"}},
		{"embedded_field_known", `{"shared":"x"}`, nil},
		{"embedded_pointer_field_known", `{"deep":"x"}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UnknownKeys([]byte(tc.data), testOuter{})
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.expected, got)
		})
	}

	t.Run("malformed_json_errors", func(t *testing.T) {
		_, err := UnknownKeys([]byte(`{`), testOuter{})
		assert.Error(t, err)
	})
}
