package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		data    string // empty parses as an empty object
		wantErr bool
		want    string
	}{
		{"empty_input_is_empty_object", "", false, "{}"},
		{"whitespace_only_is_empty_object", " \n\t", false, "{}"},
		{"trailing_whitespace_ok", `{"a":1}` + "\n ", false, `{"a":1}`},
		{"scalar_parses", `42`, false, `42`},
		{"trailing_garbage_errors", `{"a":1} junk`, true, ""},
		{"second_value_errors", `{} {}`, true, ""},
		{"extra_closing_brace_errors", `{"a":1}}`, true, ""},
		{"scalar_then_scalar_errors", `1 2`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parseNode([]byte(tc.data))
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(root.marshal()))
		})
	}
}

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
		// a new key is appended after existing ones, unknown keys keep their place
		out, err := SetKey([]byte(`{"z":1,"a":{"b":2}}`), "m", 3)
		require.NoError(t, err)
		assert.Less(t, strings.Index(string(out), `"z"`), strings.Index(string(out), `"m"`))
	})

	t.Run("duplicate_key_round_trips_once", func(t *testing.T) {
		// a source file that repeats a key keeps the last value; the key must
		// not be emitted twice on Save
		root, err := parseNode([]byte(`{"a":1,"a":2}`))
		require.NoError(t, err)
		out := root.marshal()
		assert.Equal(t, `{"a":2}`, string(out))
	})

	t.Run("unserializable_value_errors", func(t *testing.T) {
		// a value json.Marshal rejects surfaces as an error rather than corrupting the layer
		_, err := SetKey([]byte(`{}`), "a", make(chan int))
		assert.Error(t, err)
	})
}

func TestDeleteKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
		key  string
		want string
	}{
		{
			"removes_leaf",
			`{"model":"m","reasoning":{"level":"low"}}`, "model",
			`{"reasoning":{"level":"low"}}`,
		},
		{
			"removes_nested_leaf",
			`{"tools":{"limits":{"bash":{"lines":10,"bytes":20}}}}`, "tools.limits.bash.lines",
			`{"tools":{"limits":{"bash":{"bytes":20}}}}`,
		},
		{
			"prunes_emptied_ancestors",
			`{"model":"m","tools":{"limits":{"bash":{"lines":10}}}}`, "tools.limits.bash.lines",
			`{"model":"m"}`,
		},
		{
			"keeps_populated_ancestors",
			`{"tools":{"enabled":["read"],"limits":{"bash":{"lines":10}}}}`, "tools.limits.bash.lines",
			`{"tools":{"enabled":["read"]}}`,
		},
		{
			"missing_key_unchanged",
			`{"model":"m"}`, "reasoning.level",
			`{"model":"m"}`,
		},
		{
			"scalar_intermediate_unchanged",
			`{"a":5}`, "a.b",
			`{"a":5}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := DeleteKey([]byte(tc.data), tc.key)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(out))
		})
	}

	t.Run("non_object_document_unchanged", func(t *testing.T) {
		out, err := DeleteKey([]byte(`[1,2]`), "a")
		require.NoError(t, err)
		assert.JSONEq(t, `[1,2]`, string(out))
	})

	t.Run("empty_data_unchanged", func(t *testing.T) {
		out, err := DeleteKey(nil, "a")
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	t.Run("preserves_sibling_order", func(t *testing.T) {
		// tools keeps a child, so the emptied enabled leaf drops but the subtree stays
		out, err := DeleteKey([]byte(`{"z":1,"tools":{"enabled":["read"],"find":{"lines":5}},"a":2}`), "tools.enabled")
		require.NoError(t, err)
		s := string(out)
		assert.Less(t, strings.Index(s, `"z"`), strings.Index(s, `"tools"`))
		assert.Less(t, strings.Index(s, `"tools"`), strings.Index(s, `"a"`))
	})
}
