package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelaxJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  any
	}{
		{"plain_json_untouched", `{"a":1}`, map[string]any{"a": float64(1)}},
		{"trailing_comma_in_object", `{"a":1,}`, map[string]any{"a": float64(1)}},
		{"trailing_comma_in_array", `{"a":[1,2,]}`, map[string]any{"a": []any{float64(1), float64(2)}}},
		{"trailing_comma_before_newline", "{\"a\":1,\n}", map[string]any{"a": float64(1)}},
		{"nested_trailing_commas", `{"a":{"b":[1,],},}`, map[string]any{"a": map[string]any{"b": []any{float64(1)}}}},
		{"line_comment_on_its_own", "{\n// note\n\"a\":1}", map[string]any{"a": float64(1)}},
		{"line_comment_after_a_value", "{\"a\":1 // note\n}", map[string]any{"a": float64(1)}},
		{"comment_then_trailing_comma", "{\"a\":1, // note\n}", map[string]any{"a": float64(1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			require.NoError(t, json.Unmarshal(RelaxJSON([]byte(tc.input)), &got))
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("string_contents_are_untouched", func(t *testing.T) {
		// the whole point of scanning rather than pattern matching
		in := `{"a":"// not a comment","b":"trailing, ]","c":"quote \" then , }"}`
		var got map[string]string
		require.NoError(t, json.Unmarshal(RelaxJSON([]byte(in)), &got))
		assert.Equal(t, "// not a comment", got["a"])
		assert.Equal(t, "trailing, ]", got["b"])
		assert.Equal(t, `quote " then , }`, got["c"])
	})
	t.Run("a_url_is_not_a_comment", func(t *testing.T) {
		in := `{"baseUrl":"http://localhost:1234/v1"}`
		var got map[string]string
		require.NoError(t, json.Unmarshal(RelaxJSON([]byte(in)), &got))
		assert.Equal(t, "http://localhost:1234/v1", got["baseUrl"])
	})
	t.Run("offsets_are_preserved", func(t *testing.T) {
		// blanking rather than deleting is what lets errors point at the
		// original file
		in := []byte("{\"a\":1, // note\n}")
		assert.Len(t, RelaxJSON(in), len(in))
	})
	t.Run("input_is_not_mutated", func(t *testing.T) {
		in := []byte(`{"a":1,}`)
		RelaxJSON(in)
		assert.Equal(t, `{"a":1,}`, string(in))
	})
	t.Run("comma_before_a_real_value_is_kept", func(t *testing.T) {
		var got map[string]any
		require.NoError(t, json.Unmarshal(RelaxJSON([]byte(`{"a":1,"b":2}`)), &got))
		assert.Len(t, got, 2)
	})
}

func TestJSONError(t *testing.T) {
	t.Parallel()

	t.Run("names_the_line_and_column", func(t *testing.T) {
		data := []byte("{\n  \"a\": 1\n  \"b\": 2\n}")
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		got := JSONError("models.json", data, err).Error()
		assert.Contains(t, got, "models.json:3:")
		assert.Contains(t, got, `"b": 2`) // the offending line
		assert.Contains(t, got, "^")
	})
	t.Run("type_errors_are_located_too", func(t *testing.T) {
		data := []byte("{\n  \"a\": \"text\"\n}")
		var v struct {
			A int `json:"a"`
		}
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		assert.Contains(t, JSONError("models.json", data, err).Error(), "models.json:2:")
	})
	t.Run("errors_without_an_offset_still_name_the_file", func(t *testing.T) {
		got := JSONError("models.json", nil, assert.AnError).Error()
		assert.Contains(t, got, "models.json")
	})
	t.Run("wraps_the_original", func(t *testing.T) {
		data := []byte("{")
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)
		assert.ErrorIs(t, JSONError("m.json", data, err), err)
	})
}

func TestLocate(t *testing.T) {
	t.Parallel()

	data := []byte("one\ntwo\nthree")
	tests := []struct {
		name   string
		offset int64
		line   int
		col    int
		text   string
	}{
		{"first_byte", 0, 1, 1, "one"},
		{"mid_first_line", 2, 1, 3, "one"},
		{"start_of_second", 4, 2, 1, "two"},
		{"third_line", 8, 3, 1, "three"},
		{"past_the_end_is_clamped", 999, 3, 6, "three"},
		{"negative_is_clamped", -5, 1, 1, "one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, col, text := locate(data, tc.offset)
			assert.Equal(t, tc.line, line)
			assert.Equal(t, tc.col, col)
			assert.Equal(t, tc.text, text)
		})
	}
}

func TestDuplicateKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"none", `{"a":1,"b":2}`, nil},
		{"top_level", `{"a":1,"a":2}`, []string{"a"}},
		{"nested", `{"o":{"k":1,"k":2}}`, []string{"o.k"}},
		{"inside_an_array", `{"m":[{"c":{"f":1,"f":2}}]}`, []string{"m[].c.f"}},
		{"several", `{"a":1,"a":2,"b":3,"b":4}`, []string{"a", "b"}},
		{"same_key_in_sibling_objects_is_fine", `{"x":{"k":1},"y":{"k":2}}`, nil},
		{"same_key_across_array_items_is_fine", `[{"k":1},{"k":2}]`, nil},
		{"scalars_and_empties", `{"a":[],"b":{},"c":null}`, nil},
		{"malformed_reports_nothing", `{`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, DuplicateKeys([]byte(tc.input)))
		})
	}

	t.Run("the_pi_compat_case", func(t *testing.T) {
		// a real pi models.json declares thinkingFormat twice in one compat block
		in := `{"providers":{"lmstudio":{"models":[{"compat":{` +
			`"thinkingFormat":"reasoning_effort","maxTokensField":"max_tokens",` +
			`"thinkingFormat":"deepseek"}}]}}}`
		assert.Equal(t,
			[]string{"providers.lmstudio.models[].compat.thinkingFormat"},
			DuplicateKeys([]byte(in)))
	})
}
