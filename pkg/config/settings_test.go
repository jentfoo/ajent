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
		Layer{Name: "user", Data: []byte(`{
			"model":"claude",
			"reasoning":{"level":"low"},
			"compaction":{"auto":false,"threshold":0.4}
		}`)},
	)
	require.NoError(t, err)

	var st Settings
	require.NoError(t, json.Unmarshal(r.Bytes(), &st))
	assert.Equal(t, "claude", st.Model)        // user sets a field absent from defaults
	assert.Equal(t, "low", st.Reasoning.Level) // overrides the default level
	assert.False(t, st.Compaction.Auto)        // overrides the default true
	assert.InDelta(t, 0.4, st.Compaction.Threshold, 1e-9)
	// fields the user layer leaves alone keep their defaults
	assert.Equal(t, "wholeTurn", st.Reasoning.Retain) // default retain name survives the level override
	assert.Equal(t, "auto", st.UI.Render)
}

func TestAgentMaxStepsLayers(t *testing.T) {
	t.Parallel()

	// no compiled-in cap: the key is absent from defaults, so the zero value
	// (unlimited) is what an unconfigured install runs with.
	var base Settings
	require.NoError(t, json.Unmarshal(Defaults().Data, &base))
	assert.Zero(t, base.Agent.MaxSteps)
	baseResolved, err := Merge(Defaults())
	require.NoError(t, err)
	_, _, ok := baseResolved.Explain("agent.maxSteps")
	assert.False(t, ok) // reports "(default)" in /settings terms

	r, err := Merge(
		Defaults(),
		Layer{Name: "user", Data: []byte(`{"agent":{"maxSteps":50}}`)},
	)
	require.NoError(t, err)

	var st Settings
	require.NoError(t, json.Unmarshal(r.Bytes(), &st))
	assert.Equal(t, 50, st.Agent.MaxSteps) // the user layer caps it
}
