package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergePrecedenceAndExplain(t *testing.T) {
	t.Parallel()

	r, err := Merge(
		Layer{Name: "default", Data: []byte(`{"model":"d","ui":{"render":"auto"},"reasoning":{"level":"medium"}}`)},
		Layer{Name: "user", Data: []byte(`{"model":"u","compaction":{"auto":true}}`)},
		Layer{Name: "project", Data: []byte(`{"reasoning":{"level":"high"}}`)},
	)
	require.NoError(t, err)

	t.Run("top_level_precedence", func(t *testing.T) {
		v, _, ok := r.Explain("model")
		require.True(t, ok)
		assert.Equal(t, `"u"`, string(v))
		assert.Equal(t, "user", r.Source("model"))
	})
	t.Run("nested_path_provenance", func(t *testing.T) {
		v, src, ok := r.Explain("reasoning.level")
		require.True(t, ok)
		assert.Equal(t, `"high"`, string(v))
		assert.Equal(t, "project", src)
	})
	t.Run("default_only_key_present", func(t *testing.T) {
		_, _, ok := r.Explain("ui.render") // only in defaults
		require.True(t, ok)
	})
	t.Run("missing_key_not_found", func(t *testing.T) {
		v, src, ok := r.Explain("no.such.key")
		assert.False(t, ok)
		assert.Nil(t, v)
		assert.Empty(t, src)
		assert.Empty(t, r.Source("no.such.key"))
	})
}

func TestMergeDeepObjectsAndArrayReplace(t *testing.T) {
	t.Parallel()

	r, err := Merge(
		Layer{Name: "default", Data: []byte(`{
			"providers":{"a":{"baseUrl":"x","timeouts":{"connect":5}}},
			"tools":{"enabled":["read"],"limits":{"bash":{"lines":1,"bytes":2}}}
		}`)},
		Layer{Name: "user", Data: []byte(`{
			"providers":{"a":{"apiKeyEnv":"K","timeouts":{"idle":9}}},
			"tools":{"enabled":["read","bash"],"limits":{"bash":{"bytes":4}}}
		}`)},
	)
	require.NoError(t, err)

	t.Run("deep_objects_merge", func(t *testing.T) {
		var merged map[string]any
		require.NoError(t, json.Unmarshal(r.Bytes(), &merged))

		prov := merged["providers"].(map[string]any)["a"].(map[string]any)
		assert.Equal(t, "x", prov["baseUrl"])   // kept from the lower layer
		assert.Equal(t, "K", prov["apiKeyEnv"]) // added by the upper layer
		tm := prov["timeouts"].(map[string]any)
		assert.InDelta(t, float64(5), tm["connect"], 0) // survives from the lower layer
		assert.InDelta(t, float64(9), tm["idle"], 0)    // added by the upper layer

		// nested leaves merge: lines survive from default while bytes is replaced
		bash := merged["tools"].(map[string]any)["limits"].(map[string]any)["bash"].(map[string]any)
		assert.InDelta(t, float64(1), bash["lines"], 0)
		assert.InDelta(t, float64(4), bash["bytes"], 0)
	})
	t.Run("arrays_replace_not_merge", func(t *testing.T) {
		var merged map[string]any
		require.NoError(t, json.Unmarshal(r.Bytes(), &merged))

		enabled := merged["tools"].(map[string]any)["enabled"]
		assert.ElementsMatch(t, []string{"read", "bash"}, enabled) // exactly user's set
	})
}

func TestMergeEmptyLayerSkipped(t *testing.T) {
	t.Parallel()

	r, err := Merge(
		Defaults(),
		Layer{Name: "env"},
	)
	require.NoError(t, err)
	v, _, ok := r.Explain("reasoning.level")
	require.True(t, ok)
	assert.Equal(t, `"medium"`, string(v))
}
