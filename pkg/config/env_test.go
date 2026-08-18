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
