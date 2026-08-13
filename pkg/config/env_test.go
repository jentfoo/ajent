package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvLayerBindsScalarKinds(t *testing.T) {
	t.Parallel()

	vars := map[string]string{
		"AJENT_MODEL":                "anthropic/claude",
		"AJENT_REASONING_LEVEL":      "low",
		"AJENT_COMPACTION_AUTO":      "true",
		"AJENT_COMPACTION_THRESHOLD": "0.5",
	}
	l, warns := EnvLayer(func(k string) string { return vars[k] })
	assert.Empty(t, warns)

	var st Settings
	require.NoError(t, json.Unmarshal(l.Data, &st))
	assert.Equal(t, "anthropic/claude", st.Model)
	assert.Equal(t, "low", st.Reasoning.Level)
	assert.True(t, st.Compaction.Auto)
	assert.InDelta(t, 0.5, st.Compaction.Threshold, 1e-9)
}

func TestEnvLayerUnparseableWarns(t *testing.T) {
	t.Parallel()

	vars := map[string]string{"AJENT_COMPACTION_THRESHOLD": "notanumber"}
	l, warns := EnvLayer(func(k string) string { return vars[k] })
	assert.Len(t, warns, 1)
	assert.Contains(t, strings.Join(warns, "\n"), "want a number")
	// the bad value is skipped; the layer stays valid
	assert.NotContains(t, string(l.Data), "threshold")
}

func TestEnvLayerHomeCannotCollide(t *testing.T) {
	t.Parallel()

	vars := map[string]string{"AJENT_HOME": "/tmp/whatever"}
	l, warns := EnvLayer(func(k string) string { return vars[k] })
	assert.Empty(t, warns)
	// no `home` key exists; the var is ignored entirely
	assert.NotContains(t, string(l.Data), "home")
}
