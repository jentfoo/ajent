package llm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyOverridesProviderDeepMerge(t *testing.T) {
	t.Parallel()

	f := File{Providers: map[string]ProviderConfig{
		"anthropic": {BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
	}}
	out, warns, err := ApplyOverrides(f,
		json.RawMessage(`{"anthropic":{"baseUrl":"http://override","timeouts":{"connect":"5s"}}}`),
		nil)
	require.NoError(t, err)
	assert.Empty(t, warns)

	p := out.Providers["anthropic"]
	assert.Equal(t, "http://override", p.BaseURL)     // override wins
	assert.Equal(t, "ANTHROPIC_API_KEY", p.APIKeyEnv) // unrelated field kept
	require.NotNil(t, p.Timeouts.Connect)
	assert.EqualValues(t, 5*time.Second, *p.Timeouts.Connect)
}

func TestApplyOverridesModelSetsOneField(t *testing.T) {
	t.Parallel()

	f := File{Providers: map[string]ProviderConfig{
		"anthropic": {Models: []ModelConfig{{ID: "opus", ContextWindow: ptr(200000), Name: "Opus"}}},
	}}
	out, warns, err := ApplyOverrides(f,
		nil,
		json.RawMessage(`{"anthropic/opus":{"contextWindow":100000}}`))
	require.NoError(t, err)
	assert.Empty(t, warns)

	mc := out.Providers["anthropic"].Models[0]
	assert.InDelta(t, float64(1e5), float64(*mc.ContextWindow), 0) // override set
	assert.Equal(t, "Opus", mc.Name)                               // the rest survives
}

func TestApplyOverridesUnknownProviderWarns(t *testing.T) {
	t.Parallel()

	f := File{Providers: map[string]ProviderConfig{}}
	out, warns, err := ApplyOverrides(f,
		nil,
		json.RawMessage(`{"ghost/model":{"contextWindow":1}}`))
	require.NoError(t, err)
	assert.NotEmpty(t, warns)
	assert.Empty(t, out.Providers["ghost"].Models)
}

func TestApplyOverridesNewModelAppends(t *testing.T) {
	t.Parallel()

	f := File{Providers: map[string]ProviderConfig{
		"openrouter": {Models: []ModelConfig{{ID: "existing"}}},
	}}
	out, warns, err := ApplyOverrides(f,
		nil,
		json.RawMessage(`{"openrouter/brand-new":{"contextWindow":32000}}`))
	require.NoError(t, err)
	assert.Empty(t, warns)

	ms := out.Providers["openrouter"].Models
	require.Len(t, ms, 2)
	assert.Equal(t, "brand-new", ms[1].ID)
	require.NotNil(t, ms[1].ContextWindow)
}
