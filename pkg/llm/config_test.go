package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadFixture copies a config fixture into a private temp file and loads it,
// since a checked in file cannot carry the 0600 mode a real one would.
func loadFixture(t *testing.T, name string) (File, []string, error) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "config", name))
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), ModelsFileName)
	require.NoError(t, os.WriteFile(path, data, config.SecretPerm))
	return LoadFile(path)
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	t.Run("compat_style_file_loads_unchanged", func(t *testing.T) {
		f, warnings, err := loadFixture(t, "models_compat_style.json")
		require.NoError(t, err)
		assert.Empty(t, warnings)

		require.Len(t, f.Providers, 2)
		lms := f.Providers["lmstudio"]
		assert.Equal(t, "http://192.168.1.100:1111/v1", lms.BaseURL)
		assert.Equal(t, DialectOpenAICompletions, lms.API)
		assert.Equal(t, "lmstudio", lms.APIKey)
		require.Len(t, lms.Models, 3)

		qwen := lms.Models[0]
		assert.Equal(t, "qwen3.6-27b-mtp", qwen.ID)
		require.NotNil(t, qwen.ContextWindow)
		assert.Equal(t, 200000, *qwen.ContextWindow)
		require.NotNil(t, qwen.Reasoning)
		assert.Equal(t, ReasoningUnset, *qwen.Reasoning) // "reasoning": true
		require.NotNil(t, qwen.Compat.ThinkingFormat)
		assert.Equal(t, "qwen-chat-template", *qwen.Compat.ThinkingFormat)
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, qwen.Input)
	})
	t.Run("thinking_level_map_maps_every_level", func(t *testing.T) {
		f, _, err := loadFixture(t, "models_compat_style.json")
		require.NoError(t, err)

		deepseek := f.Providers["lmstudio"].Models[1]
		require.Len(t, deepseek.LevelMap, 7)
		require.NotNil(t, deepseek.LevelMap[LevelXHigh])
		assert.Equal(t, "high", *deepseek.LevelMap[LevelXHigh])

		minimax := f.Providers["lmstudio"].Models[2]
		require.Contains(t, minimax.LevelMap, LevelOff)
		assert.Nil(t, minimax.LevelMap[LevelOff]) // null disables reasoning at that level
	})
	t.Run("unknown_keys_warn_without_failing", func(t *testing.T) {
		f, warnings, err := loadFixture(t, "models_unknown_keys.json")
		require.NoError(t, err)
		require.Len(t, f.Providers, 1)

		joined := strings.Join(warnings, "\n")
		assert.Contains(t, joined, "bogusProviderKey")
		// cost is accepted for compatibility but pricing itself stays out of scope,
		// so it must not warn on a real config that carries it
		assert.NotContains(t, joined, "cost")
	})
	t.Run("minimal_file", func(t *testing.T) {
		f, warnings, err := loadFixture(t, "models_minimal.json")
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Equal(t, "http://localhost:8080", f.Providers["llamacpp"].BaseURL)
	})
	t.Run("missing_file_is_not_an_error", func(t *testing.T) {
		f, warnings, err := LoadFile(filepath.Join(t.TempDir(), "absent.json"))
		require.NoError(t, err)
		assert.Empty(t, f.Providers)
		assert.Empty(t, warnings)
	})
	t.Run("malformed_json_names_the_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ModelsFileName)
		require.NoError(t, os.WriteFile(path, []byte(`{"providers":`), 0o600))

		_, _, err := LoadFile(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), path)
	})
	t.Run("compat_quirks_load_unchanged", func(t *testing.T) {
		// line comments, a trailing comma and a duplicated key, exactly as
		// a real-world models.json carries them
		f, warnings, err := loadFixture(t, "models_compat_quirks.json")
		require.NoError(t, err)

		models := f.Providers["lmstudio"].Models
		require.Len(t, models, 1)
		assert.Equal(t, "deepseek-v4-flash-0731", models[0].ID)

		// the duplicate is surfaced rather than silently resolved
		joined := strings.Join(warnings, "\n")
		assert.Contains(t, joined, "duplicate key")
		assert.Contains(t, joined, "thinkingFormat")
	})
	t.Run("syntax_error_names_the_line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ModelsFileName)
		require.NoError(t, os.WriteFile(path,
			[]byte("{\n  \"providers\": {\n    \"a\": 1\n    \"b\": 2\n  }\n}"), 0o600))

		_, _, err := LoadFile(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), path+":4:")
	})
	t.Run("literal_key_in_readable_file_warns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ModelsFileName)
		require.NoError(t, os.WriteFile(path,
			[]byte(`{"providers":{"a":{"apiKey":"sk-secret"}}}`), 0o644))

		_, warnings, err := LoadFile(path)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(warnings, "\n"), "chmod 600")
	})
	t.Run("no_literal_key_no_permission_warning", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ModelsFileName)
		require.NoError(t, os.WriteFile(path,
			[]byte(`{"providers":{"a":{"apiKeyEnv":"A_KEY"}}}`), 0o644))

		_, warnings, err := LoadFile(path)
		require.NoError(t, err)
		assert.Empty(t, warnings)
	})
}

// TestModelConfigCompatParity loads a compatibility-style model entry carrying the fields ajent
// accepts for parity: per-model headers, opaque sampling params and a cost block
// that is out of scope but must not warn.
func TestModelConfigCompatParity(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ModelsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{
  "providers": {
    "llamacpp": {
      "baseUrl": "http://localhost:8080",
      "name": "Local llama.cpp",
      "oauth": "radius",
      "authHeader": true,
      "models": [{
        "id": "qwen3.6-27b-mtp",
        "headers": { "X-Org": "acme" },
        "samplingParams": { "temperature": 0.2, "seed": 7 },
        "cost": { "input": 1.00, "output": 3.40 }
      }]
    }
  }
}`), config.SecretPerm))

	f, warnings, err := LoadFile(path)
	require.NoError(t, err)
	assert.Empty(t, warnings) // cost and the accepted provider keys must not warn

	p := f.Providers["llamacpp"]
	assert.Equal(t, "Local llama.cpp", p.Name)
	assert.Equal(t, "radius", p.OAuth)
	require.NotNil(t, p.AuthHeader)
	assert.True(t, *p.AuthHeader)

	m := p.Models[0]
	assert.Equal(t, map[string]string{"X-Org": "acme"}, m.Headers)
	assert.InDelta(t, 0.2, m.SamplingParams["temperature"], 1e-9)
	require.NotNil(t, m.Cost)
}

func TestDialectUnmarshalText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected Dialect
	}{
		{"anthropic", "anthropic", DialectAnthropic},
		{"openai_responses", "openai-responses", DialectOpenAIResponses},
		{"openai_completions", "openai-completions", DialectOpenAICompletions},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Dialect
			require.NoError(t, got.UnmarshalText([]byte(tc.input)))
			assert.Equal(t, tc.expected, got)
			assert.Equal(t, tc.input, got.String())
		})
	}

	t.Run("unknown_errors", func(t *testing.T) {
		var got Dialect
		assert.Error(t, got.UnmarshalText([]byte("grpc")))
	})
}

func TestFlavorUnmarshalText(t *testing.T) {
	t.Parallel()

	var got Flavor
	require.NoError(t, got.UnmarshalText([]byte("lmstudio")))
	assert.Equal(t, FlavorLMStudio, got)
	assert.Equal(t, "lmstudio", got.String())
}
