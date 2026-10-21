package config

import (
	"encoding/json"
	"strings"
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
		// blanking rather than deleting is what lets errors point at the original file
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

func TestRelaxJSONReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		hasComments bool
	}{
		{"plain_json", `{"a":1}`, false},
		{"trailing_comma_only", `{"a":1,}`, false},
		{"nested_trailing_commas_only", `{"a":{"b":[1,],},}`, false},
		{"comment_only", "{\n// note\n}", true},
		{"comment_beside_trailing_comma", "{\"a\":1, // note\n}", true},
		{"comment_spelled_in_a_string", `{"a":"// not a comment"}`, false},
		{"url_slashes_are_not_a_comment", `{"baseUrl":"http://host//path"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, hasComments := RelaxJSONReport([]byte(tc.input))
			assert.Equal(t, tc.hasComments, hasComments)
			assert.Len(t, out, len(tc.input)) // blanking, so offsets survive like RelaxJSON
		})
	}

	t.Run("output_matches_relax_json", func(t *testing.T) {
		in := []byte("{\"a\":1, // note\n}")
		out, _ := RelaxJSONReport(in)
		assert.Equal(t, string(RelaxJSON(in)), string(out))
	})
}

func TestRelaxedObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantDoc string // expected returned JSON; empty means nil data
	}{
		{"empty_file_is_absent", "", ""},
		{"whitespace_only_is_absent", " \t\n ", ""},
		{"comment_only_is_absent", "// nothing yet\n// still nothing\n", ""},
		{"object_passes_through_relaxed", "\n  {\"a\":1}\n", "\n  {\"a\":1}\n"},
		// the comment is blanked, not deleted, so byte offsets survive
		{"comment_inside_object_is_blanked", "{ // note\n}", "{" + strings.Repeat(" ", 8) + "\n}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := RelaxedObject("settings.json", []byte(tc.input))
			require.NoError(t, err)
			if tc.wantDoc == "" {
				assert.Nil(t, data)
			} else {
				assert.Equal(t, tc.wantDoc, string(data))
			}
		})
	}

	errTests := []struct {
		name      string
		input     string
		errSubstr string
	}{
		{"null_document", "null", ":1:1: expected a JSON object at the top level, found null"},
		{"array_document", "[1]", "found an array"},
		{"string_document", `"x"`, "found a string"},
		{"number_document", "123", "found a number"},
		{"boolean_document", "true", "found a boolean"},
		{"comment_then_null_names_line", "// tuned by hand\nnull", ":2:1: expected a JSON object at the top level, found null"},
	}
	for _, tc := range errTests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := RelaxedObject("settings.json", []byte(tc.input))
			require.Error(t, err)
			assert.Nil(t, data)
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
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

	t.Run("secret_value_is_redacted", func(t *testing.T) {
		data := []byte("{\n  \"apiKey\": \"sk-super-secret\" @oops\n}")
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		got := JSONError("models.json", data, err).Error()
		assert.Contains(t, got, "models.json:2:")
		assert.NotContains(t, got, "sk-super-secret")
		assert.Contains(t, got, `"apiKey": [redacted]`)
	})

	t.Run("minified_file_masks_mid_line", func(t *testing.T) {
		data := []byte(`{"providers":{"openai":{"apiKey":"sk-super-secret" "model":"gpt"}}}`)
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		got := JSONError("models.json", data, err).Error()
		assert.Contains(t, got, "models.json:1:")
		assert.NotContains(t, got, "sk-super-secret")
		assert.Contains(t, got, `[redacted]`)
		assert.Contains(t, got, `"model":"gpt"`) // structure around the secret stays visible
	})

	t.Run("minified_missing_colon_masks", func(t *testing.T) {
		data := []byte(`{"apiKey" "sk-super-secret"}`)
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		got := JSONError("m.json", data, err).Error()
		assert.NotContains(t, got, "sk-super-secret")
	})

	t.Run("non_secret_lines_are_untouched", func(t *testing.T) {
		data := []byte("{\n  \"tokenfile\": \"keep me\" @oops\n}")
		var v map[string]any
		err := json.Unmarshal(data, &v)
		require.Error(t, err)

		got := JSONError("m.json", data, err).Error()
		assert.Contains(t, got, `"tokenfile": "keep me"`)
	})
}

func TestRedactSecretLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{"plain_line_untouched", `{"a": 1, "b": "two"}`, `{"a": 1, "b": "two"}`},
		{"key_at_line_start", `"apiKey": "sk-123" @oops`, `"apiKey": [redacted] @oops`},
		{"indented_key_first", "  \"token\": \"tk-1\", // note", "  \"token\": [redacted], // note"},
		{"minified_mid_line", `{"p":{"apiKey":"sk-1"},"m":"gpt"}`, `{"p":{"apiKey":[redacted]},"m":"gpt"}`},
		{"multiple_keys_masked", `{"a":1,"apiKey":"sk-1","b":2,"token":"tk-2"}`, `{"a":1,"apiKey":[redacted],"b":2,"token":[redacted]}`},
		{"nested_secret_key", `{"x":{"secret":"s-1"}}`, `{"x":{"secret":[redacted]}}`},
		{"escaped_quotes_in_value", `{"apiKey":"sk-\"123\""} @oops`, `{"apiKey":[redacted]} @oops`},
		{"unterminated_string_value", `  "apiKey": "sk-1`, `  "apiKey": [redacted]`},
		{"bare_value_masks_rest", `{"apiKey": 123, "m": "gpt"}`, `{"apiKey": [redacted]`},
		{"missing_colon_masks_rest", `{"apiKey" "sk-1","b":1}`, `{"apiKey" [redacted]`},
		{"key_without_value_untouched", `  "apiKey"`, `  "apiKey"`},
		{"longer_key_name_kept", `{"tokenfile": "keep me"}`, `{"tokenfile": "keep me"}`},
		// a quote after a colon is a value spelling a key name, never a key
		{"value_spelling_key_name", `{"model": "token"}`, `{"model": "token"}`},
		{"value_spelling_key_minified", `{"m":"token"}`, `{"m":"token"}`},
		{"secret_masked_beside_exempt_value", `{"apiKey":"sk-1","m":"token"}`, `{"apiKey":[redacted],"m":"token"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, redactSecretLine(tc.line))
		})
	}
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

	t.Run("duplicate_key_in_compat_warns", func(t *testing.T) {
		// a hand-written models.json declares thinkingFormat twice in one compat block
		in := `{"providers":{"lmstudio":{"models":[{"compat":{` +
			`"thinkingFormat":"reasoning_effort","maxTokensField":"max_tokens",` +
			`"thinkingFormat":"deepseek"}}]}}}`
		assert.Equal(t,
			[]string{"providers.lmstudio.models[].compat.thinkingFormat"},
			DuplicateKeys([]byte(in)))
	})
}
