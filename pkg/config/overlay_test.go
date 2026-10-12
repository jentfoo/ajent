package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ovModel mirrors a schema with an identified array element type.
type ovModel struct {
	ID     string `json:"id"`
	Window *int   `json:"window,omitempty"`
}

// ovProvider is one named entry holding a model list.
type ovProvider struct {
	Key      string    `json:"key,omitempty"`
	Disabled bool      `json:"disabled,omitempty"` // declared, exercised when over omits it
	Models   []ovModel `json:"models,omitempty"`
}

// ovFile is the schema OverlayJSON tests graft against: struct fields, a map of
// structs and a raw passthrough.
type ovFile struct {
	Version   int                   `json:"version,omitempty"`
	Providers map[string]ovProvider `json:"providers"`
	Raw       json.RawMessage       `json:"raw,omitempty"`
}

// overlay runs OverlayJSON with the test schema and returns parsed output.
func overlay(t *testing.T, base, over string) map[string]any {
	t.Helper()

	out, err := OverlayJSON([]byte(base), []byte(over), ovFile{})
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	return doc
}

func TestOverlayJSON(t *testing.T) {
	t.Parallel()

	over := `{"providers":{"zai":{"key":"k","models":[{"id":"glm-5"}]}}}`

	t.Run("empty_base_writes_over", func(t *testing.T) {
		out, err := OverlayJSON(nil, []byte(over), ovFile{})
		require.NoError(t, err)
		want, err := json.MarshalIndent(ovFile{Providers: map[string]ovProvider{
			"zai": {Key: "k", Models: []ovModel{{ID: "glm-5"}}},
		}}, "", "  ")
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(out))
	})

	t.Run("unknown_keys_survive_at_every_level", func(t *testing.T) {
		base := `{
			"handwrittenNote": true,
			"providers": {"zai": {
				"futureField": 7,
				"models": [{"id": "glm-5", "vendorHint": "beta"}]
			}}
		}`
		doc := overlay(t, base, over)

		assert.Equal(t, true, doc["handwrittenNote"])
		zai := doc["providers"].(map[string]any)["zai"].(map[string]any)
		assert.InDelta(t, 7.0, zai["futureField"], 0)
		model := zai["models"].([]any)[0].(map[string]any)
		assert.Equal(t, "beta", model["vendorHint"])
	})

	t.Run("declared_but_absent_fields_stay_absent", func(t *testing.T) {
		base := `{"providers": {"zai": {"key": "old", "disabled": true}}}`
		doc := overlay(t, base, `{"providers":{"zai":{"key":"k"}}}`)

		zai := doc["providers"].(map[string]any)["zai"].(map[string]any)
		assert.Equal(t, "k", zai["key"]) // governed by over
		assert.NotContains(t, zai, "disabled")
	})

	t.Run("case_insensitive_known_keys_are_governed", func(t *testing.T) {
		base := `{"providers": {"zai": {"KEY": "old"}}}`
		doc := overlay(t, base, over)

		zai := doc["providers"].(map[string]any)["zai"].(map[string]any)
		assert.NotContains(t, zai, "KEY")
		assert.Equal(t, "k", zai["key"])
	})

	t.Run("unmatched_elements_and_providers_drop", func(t *testing.T) {
		base := `{"providers": {"gone": {"key": "old"}, "zai": {
			"models": [{"id": "other", "vendorHint": "lost"}]
		}}}`
		doc := overlay(t, base, over)

		assert.NotContains(t, doc["providers"].(map[string]any), "gone")
		zai := doc["providers"].(map[string]any)["zai"].(map[string]any)
		require.Len(t, zai["models"].([]any), 1) // id does not match, over stands
	})

	t.Run("unknown_subtree_copies_whole", func(t *testing.T) {
		base := `{"providers": {"zai": {"extra": {"deep": [{"x": 1}, "y"]}}}}`
		doc := overlay(t, base, over)

		zai := doc["providers"].(map[string]any)["zai"].(map[string]any)
		extra, ok := zai["extra"].(map[string]any)
		require.True(t, ok)
		assert.Len(t, extra["deep"], 2)
	})

	t.Run("raw_field_is_governed_by_over", func(t *testing.T) {
		base := `{"raw": {"base": true}}`
		doc := overlay(t, base, `{"raw": {"over": true}}`)

		assert.Equal(t, map[string]any{"over": true}, doc["raw"])
	})

	t.Run("duplicate_ids_pair_once", func(t *testing.T) {
		base := `{"providers": {"zai": {"models": [
			{"id": "glm-5", "vendorHint": "first"},
			{"id": "glm-5", "vendorHint": "second"}
		]}}}`
		doc := overlay(t, base, over)

		models := doc["providers"].(map[string]any)["zai"].(map[string]any)["models"].([]any)
		require.Len(t, models, 1)
		assert.Equal(t, "first", models[0].(map[string]any)["vendorHint"])
	})

	t.Run("non_string_or_missing_ids_pair_nothing", func(t *testing.T) {
		base := `{"providers": {"zai": {"models": [
			{"id": 5, "vendorHint": "lost"},
			{"name": "no id", "vendorHint": "lost"}
		]}}}`
		doc := overlay(t, base, over)

		models := doc["providers"].(map[string]any)["zai"].(map[string]any)["models"].([]any)
		require.Len(t, models, 1)
		assert.NotContains(t, models[0].(map[string]any), "vendorHint")
	})

	t.Run("malformed_base_is_an_error", func(t *testing.T) {
		_, err := OverlayJSON([]byte("{ nope"), []byte(over), ovFile{})
		require.Error(t, err)
	})

	t.Run("malformed_over_is_an_error", func(t *testing.T) {
		_, err := OverlayJSON(nil, []byte("{ nope"), ovFile{})
		require.Error(t, err)
	})
}
