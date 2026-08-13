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

	v1, _, ok := r.Explain("model")
	require.True(t, ok)
	assert.Equal(t, `"u"`, string(v1))
	assert.Equal(t, "user", r.Source("model"))

	v, src, ok := r.Explain("reasoning.level")
	require.True(t, ok)
	assert.Equal(t, `"high"`, string(v))
	assert.Equal(t, "project", src)

	_, _, ok = r.Explain("ui.render") // only in defaults
	require.True(t, ok)
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

	var merged map[string]any
	require.NoError(t, json.Unmarshal(r.Bytes(), &merged))

	prov := merged["providers"].(map[string]any)["a"].(map[string]any)
	assert.Equal(t, "x", prov["baseUrl"])   // kept from the lower layer
	assert.Equal(t, "K", prov["apiKeyEnv"]) // added by the upper layer
	tm := prov["timeouts"].(map[string]any)
	assert.InDelta(t, float64(5), tm["connect"], 0)
	assert.InDelta(t, float64(9), tm["idle"], 0)

	// arrays replace wholesale: enabled is exactly user's set
	tools := merged["tools"].(map[string]any)
	assert.ElementsMatch(t, []string{"read", "bash"}, tools["enabled"])
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
