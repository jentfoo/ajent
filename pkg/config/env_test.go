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

	cases := []struct {
		name    string
		varName string
		value   string
		want    any
		get     func(Settings) any
	}{
		{"string", "AJENT_MODEL", "anthropic/claude", "anthropic/claude", func(s Settings) any { return s.Model }},
		{"bool", "AJENT_COMPACTION_AUTO", "true", true, func(s Settings) any { return s.Compaction.Auto }},
		{"float", "AJENT_COMPACTION_THRESHOLD", "0.5", float64(0.5), func(s Settings) any { return s.Compaction.Threshold }},
		{"int", "AJENT_SUBAGENT_MAXCONCURRENT", "2", 2, func(s Settings) any { return s.Subagent.MaxConcurrent }},
		{"min_steps", "AJENT_COMPACTION_MINSTEPS", "3", 3, func(s Settings) any { return s.Compaction.MinSteps }},
		{"verbatim_fraction", "AJENT_COMPACTION_VERBATIMFRACTION", "0.2", float64(0.2), func(s Settings) any { return s.Compaction.VerbatimFraction }},
		{"disable_update_check", "AJENT_DISABLEUPDATECHECK", "true", true, func(s Settings) any { return s.DisableUpdateCheck }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, warns := EnvLayer(func(k string) string {
				if k == tc.varName {
					return tc.value
				}
				return ""
			})
			assert.Empty(t, warns)

			var st Settings
			require.NoError(t, json.Unmarshal(l.Data, &st))
			assert.Equal(t, tc.want, tc.get(st))
		})
	}
}

func TestEnvLayerUnparseableWarns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		varName string
		value   string
		wantSub string
		skipKey string // leaf json key that must be absent after the skip
	}{
		{"bad_bool", "AJENT_COMPACTION_AUTO", "maybe", "want true or false", "auto"},
		{"bad_int", "AJENT_SUBAGENT_MAXCONCURRENT", "two", "want an integer", "maxConcurrent"},
		{"bad_float", "AJENT_COMPACTION_THRESHOLD", "notanumber", "want a number", "threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, warns := EnvLayer(func(k string) string {
				if k == tc.varName {
					return tc.value
				}
				return ""
			})
			assert.Len(t, warns, 1)
			assert.Contains(t, strings.Join(warns, "\n"), tc.wantSub)
			// the bad value is skipped; the layer stays valid
			assert.NotContains(t, string(l.Data), tc.skipKey)
		})
	}
}

func TestEnvLayerHomeCannotCollide(t *testing.T) {
	t.Parallel()

	vars := map[string]string{"AJENT_HOME": "/tmp/whatever"}
	l, warns := EnvLayer(func(k string) string { return vars[k] })
	assert.Empty(t, warns)
	// no `home` key exists; the var is ignored entirely
	assert.NotContains(t, string(l.Data), "home")
}

func TestEnvLayerKeepsOriginalCaseKeys(t *testing.T) {
	t.Parallel()

	vars := map[string]string{
		"AJENT_AGENT_MAXSTEPS":               "50",
		"AJENT_COMPACTION_MINSTEPS":          "3",
		"AJENT_UI_SHOWCOST":                  "true",
		"AJENT_TOOLS_LIMITS_REFINJECT_LINES": "1000",
	}
	l, warns := EnvLayer(func(k string) string { return vars[k] })
	assert.Empty(t, warns)

	// camelCase leaves bind at their exact-case path; no lowercased twin appears.
	var merged map[string]any
	require.NoError(t, json.Unmarshal(l.Data, &merged))
	agent := merged["agent"].(map[string]any)
	assert.InDelta(t, float64(50), agent["maxSteps"], 0)
	assert.NotContains(t, agent, "maxsteps")
	compaction := merged["compaction"].(map[string]any)
	assert.InDelta(t, float64(3), compaction["minSteps"], 0)
	assert.NotContains(t, compaction, "minsteps")
	ui := merged["ui"].(map[string]any)
	assert.Equal(t, true, ui["showCost"])
	assert.NotContains(t, ui, "showcost")
	tools := merged["tools"].(map[string]any)["limits"].(map[string]any)
	refInject := tools["refInject"].(map[string]any)
	assert.InDelta(t, float64(1000), refInject["lines"], 0)
}

func TestEnvLayerOverridesDefaultWithProvenance(t *testing.T) {
	t.Parallel()

	env := Layer{Name: "env", Data: []byte(`{"compaction":{"minSteps":3,"verbatimFraction":0.2},"ui":{"showCost":true}}`)}
	r, err := Merge(Defaults(), env)
	require.NoError(t, err)

	// the camelCase key overrides in place and keeps its exact-case spelling.
	v, src, ok := r.Explain("compaction.minSteps")
	require.True(t, ok)
	assert.Equal(t, `3`, string(v))
	assert.Equal(t, "env", src)
	v, src, ok = r.Explain("ui.showCost")
	require.True(t, ok)
	assert.Equal(t, `true`, string(v))
	assert.Equal(t, "env", src)

	// no lowercased sibling survives in the merged bytes.
	_, _, ok = r.Explain("compaction.minsteps")
	assert.False(t, ok)
	assert.NotContains(t, string(r.Bytes()), "minsteps")
}
