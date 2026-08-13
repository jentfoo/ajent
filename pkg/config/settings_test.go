package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsLiteralHasNoUnknownKeys(t *testing.T) {
	t.Parallel()

	unknown, err := UnknownKeys(Defaults().Data, Settings{})
	require.NoError(t, err)
	assert.Empty(t, unknown)
}

func TestSettingsDecodesMergedConfig(t *testing.T) {
	t.Parallel()

	r, err := Merge(
		Layer{Name: "default", Data: []byte(defaultsJSON)},
		Layer{Name: "user", Data: []byte(`{"reasoning":{"level":"low"},"tools":{"enabled":["read","bash"]}}`)},
	)
	require.NoError(t, err)

	var st Settings
	require.NoError(t, json.Unmarshal(r.Bytes(), &st))
	assert.Equal(t, "low", st.Reasoning.Level) // user overrides the default level
}
