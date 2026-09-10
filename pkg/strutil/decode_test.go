package strutil

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type decParams struct {
	Path  string   `json:"path"`
	Edits []decOp  `json:"edits"`
	Tags  []string `json:"tags"`
	Count int      `json:"count,omitempty"`
}

type decOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// decFussy fails with its own error type, exercising the generic fallback.
type decFussy struct {
	X int `json:"x"`
}

func (d *decFussy) UnmarshalJSON([]byte) error { return errors.New("decFussy internals") }

func TestDecodeArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      json.RawMessage
		wantErr string
		want    decParams
	}{
		{"top_level_array", json.RawMessage(`[1,2]`),
			"arguments must be a JSON object, but was given a JSON array", decParams{}},
		{"field_type_mismatch", json.RawMessage(`{"path":123}`),
			"path must be a string, but was given a JSON number", decParams{}},
		{"nested_field_mismatch", json.RawMessage(`{"path":"a","edits":[{"oldText":5}]}`),
			"edits.oldText must be a string, but was given a JSON number", decParams{}},
		{"array_into_object_elem", json.RawMessage(`{"path":"a","edits":[{"oldText":"x","newText":["y"]}]}`),
			"edits.newText must be a string, but was given a JSON array", decParams{}},
		{"object_into_slice_field", json.RawMessage(`{"path":"a","edits":{"oldText":"x"}}`),
			"", decParams{Path: "a", Edits: []decOp{{OldText: "x"}}}},
		{"scalar_into_scalar_elem", json.RawMessage(`{"path":"a","tags":[1]}`),
			"each item of tags must be a string, but was given a JSON number", decParams{}},
		{"scalar_into_struct_elem", json.RawMessage(`{"path":"a","edits":["x"]}`),
			"each item of edits must be a JSON object, but was given a JSON string", decParams{}},
		{"scalar_into_slice_field", json.RawMessage(`{"path":"a","tags":"x"}`),
			"", decParams{Path: "a", Tags: []string{"x"}}},
		{"mismatched_singleton_elem", json.RawMessage(`{"path":"a","edits":{"oldText":5}}`),
			"edits.oldText must be a string, but was given a JSON number", decParams{}},
		{"null_array_field_kept", json.RawMessage(`{"path":"a","edits":null}`),
			"", decParams{Path: "a"}},
		{"syntax_error", json.RawMessage(`{"path":`),
			"arguments are not valid JSON", decParams{}},
		{"valid_input", json.RawMessage(`{"path":"a.go","tags":["x"],"edits":[{"oldText":"x","newText":"y"}]}`),
			"", decParams{Path: "a.go", Tags: []string{"x"}, Edits: []decOp{{OldText: "x", NewText: "y"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p decParams
			err := DecodeArgs(tc.in, &p)
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.want, p)
				return
			}
			assert.EqualError(t, err, tc.wantErr)
		})
	}

	// bare-slice roots (edit decodes the edits value alone)
	sliceCases := []struct {
		name    string
		in      json.RawMessage
		wantErr string
	}{
		{"elem_type_mismatch", json.RawMessage(`["x"]`),
			"each element must be a JSON object, but was given a JSON string"},
		{"elem_field_mismatch", json.RawMessage(`[{"oldText":5}]`),
			"oldText must be a string, but was given a JSON number"},
		{"whole_type_mismatch", json.RawMessage(`5`),
			"expected a JSON array of objects, but was given a JSON number"},
	}
	for _, tc := range sliceCases {
		t.Run(tc.name, func(t *testing.T) {
			var ops []decOp
			assert.EqualError(t, DecodeArgs(tc.in, &ops), tc.wantErr)
		})
	}

	t.Run("custom_unmarshaler_generic", func(t *testing.T) {
		// unknown error types never leak their text
		err := DecodeArgs(json.RawMessage(`{"x":1}`), &decFussy{})
		assert.EqualError(t, err, "arguments are malformed")
	})
}
