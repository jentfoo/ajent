package command

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProbe replaces the wizard's discovery step, returning canned results and
// recording the provider names it was asked about.
func stubProbe(t *testing.T, entry llm.CacheEntry, err error) *[]string {
	t.Helper()

	probed := &[]string{}
	probeProvider = func(_ context.Context, name string, _ llm.ProviderConfig, _ llm.DiscoverOptions) (llm.CacheEntry, error) {
		*probed = append(*probed, name)
		return entry, err
	}
	t.Cleanup(func() { probeProvider = llm.ProbeProvider })
	return probed
}

// stubProbeAll replaces the custom-URL discovery step.
func stubProbeAll(t *testing.T, entry llm.CacheEntry, err error) *[]llm.ProviderConfig {
	t.Helper()

	probed := &[]llm.ProviderConfig{}
	probeAll = func(_ context.Context, cfg llm.ProviderConfig, _ llm.DiscoverOptions) (llm.CacheEntry, error) {
		*probed = append(*probed, cfg)
		return entry, err
	}
	t.Cleanup(func() { probeAll = llm.ProbeAll })
	return probed
}

// setupHome isolates the config directory so the wizard writes into a temp dir.
func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvHome, t.TempDir())
}

// pickName finds a provider choice's index by its models.json key.
func pickName(name string) int {
	for i, ch := range providerSetupChoices {
		if ch.name == name {
			return i
		}
	}
	return -1
}

// writtenFile loads what the wizard saved, failing the test when it did not.
func writtenFile(t *testing.T) llm.File {
	t.Helper()

	path, err := config.UserPath(llm.ModelsFileName)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	f, _, err := llm.LoadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, data)
	return f
}

func TestProviderSetup(t *testing.T) {
	// most cases write the user models file, so each needs its own home
	stub := llm.CacheEntry{Models: []llm.ModelConfig{{ID: "glm-5", Name: "GLM-5", ContextWindow: ptrInt(200000)}}}

	t.Run("dismissed_at_provider_pick", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)

		err := ProviderSetup(t.Context(), c)
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, c.prints)
	})

	t.Run("env_key_skips_the_prompt", func(t *testing.T) {
		setupHome(t)
		t.Setenv("ZAI_API_KEY", "envkey")
		c := newFakeConsole(t)
		probed := stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}} // provider, then model

		require.NoError(t, ProviderSetup(t.Context(), c))

		assert.Equal(t, []string{"zai"}, *probed)
		f := writtenFile(t)
		require.Contains(t, f.Providers, "zai")
		assert.Equal(t, "ZAI_API_KEY", f.Providers["zai"].APIKeyEnv)
		assert.Empty(t, f.Providers["zai"].APIKey)
		assert.Equal(t, "glm-5", f.DefaultModel)
	})

	t.Run("pasted_key_is_saved", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{"sk-secret"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Equal(t, "sk-secret", f.Providers["zai"].APIKey)
		assert.Empty(t, f.Providers["zai"].APIKeyEnv)
	})

	t.Run("empty_key_still_writes_the_entry", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{"  "}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Empty(t, f.Providers["zai"].APIKey)
	})

	t.Run("probe_failure_asks_for_a_model_id", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, llm.CacheEntry{}, errors.New("unreachable"))
		c.picks = []fakePick{{result: pickName("deepseek")}}
		c.inputs = []string{"", "deepseek-chat"} // key prompt, then model id

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		require.Len(t, f.Providers["deepseek"].Models, 1)
		assert.Equal(t, "deepseek-chat", f.Providers["deepseek"].Models[0].ID)
		assert.Equal(t, "deepseek-chat", f.DefaultModel)
		assert.True(t, c.noticeContains("discovery failed"))
	})

	t.Run("local_server_skips_the_model_prompt", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, llm.CacheEntry{}, errors.New("refused"))
		c.picks = []fakePick{{result: pickName("lmstudio")}}
		c.inputs = []string{"http://192.168.1.100:1234/v1"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Equal(t, "http://192.168.1.100:1234/v1", f.Providers["lmstudio"].BaseURL)
		assert.Empty(t, f.DefaultModel)
	})

	t.Run("bare_local_url_inherits_the_flavor_path", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, llm.CacheEntry{}, errors.New("refused"))
		c.picks = []fakePick{{result: pickName("lmstudio")}}
		c.inputs = []string{"http://192.168.1.100:1234"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Equal(t, "http://192.168.1.100:1234/v1", f.Providers["lmstudio"].BaseURL)
	})

	t.Run("scheme_less_local_url_defaults_to_http", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, llm.CacheEntry{}, errors.New("refused"))
		c.picks = []fakePick{{result: pickName("llamacpp")}}
		c.inputs = []string{"192.168.1.100:8080"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Equal(t, "http://192.168.1.100:8080", f.Providers["llamacpp"].BaseURL)
	})

	t.Run("scheme_less_custom_url_defaults_to_http", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		probed := stubProbeAll(t, stub, nil)
		c.picks = []fakePick{{result: pickName(customProviderName)}, {result: 0}}
		c.inputs = []string{"127.0.0.1:9000/v1"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		require.Len(t, *probed, 1)
		assert.Equal(t, "http://127.0.0.1:9000/v1", (*probed)[0].BaseURL)
	})

	t.Run("local_default_url_stays_unset", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("lmstudio")}, {result: 0}}
		c.inputs = []string{""} // accept the flavor default

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		assert.Empty(t, f.Providers["lmstudio"].BaseURL)
		assert.Equal(t, "glm-5", f.DefaultModel)
	})

	t.Run("custom_url_opts_into_discovery", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		probed := stubProbeAll(t, stub, nil)
		c.picks = []fakePick{{result: pickName(customProviderName)}, {result: 0}}
		c.inputs = []string{"http://127.0.0.1:9000/v1"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		require.Len(t, *probed, 1)
		assert.Equal(t, "http://127.0.0.1:9000/v1", (*probed)[0].BaseURL)
		f := writtenFile(t)
		require.Contains(t, f.Providers, customProviderName)
		assert.NotNil(t, f.Providers[customProviderName].Discover)
		assert.True(t, *f.Providers[customProviderName].Discover)
	})

	t.Run("custom_without_a_url_is_cancelled", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName(customProviderName)}}
		c.inputs = []string{""}

		err := ProviderSetup(t.Context(), c)
		assert.ErrorIs(t, err, tui.ErrCancelled)
	})

	t.Run("esc_at_model_pick_keeps_the_entry", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}} // the model pick is dismissed
		c.inputs = []string{""}                         // key prompt: empty accepted

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		require.Contains(t, f.Providers, "zai")
		assert.Empty(t, f.DefaultModel)
	})

	t.Run("esc_at_model_id_prompt_keeps_the_entry", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, llm.CacheEntry{}, errors.New("unreachable"))
		c.picks = []fakePick{{result: pickName("deepseek")}}
		c.inputs = []string{"sk-key"}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		require.Contains(t, f.Providers, "deepseek")
		assert.Empty(t, f.Providers["deepseek"].Models)
	})

	t.Run("merges_into_an_existing_file", func(t *testing.T) {
		setupHome(t)
		existing := llm.File{Providers: map[string]llm.ProviderConfig{
			"anthropic": {APIKeyEnv: "ANTHROPIC_API_KEY"},
		}}
		require.NoError(t, llm.SaveUserFile(existing))

		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{""}

		require.NoError(t, ProviderSetup(t.Context(), c))

		f := writtenFile(t)
		require.Contains(t, f.Providers, "anthropic")
		require.Contains(t, f.Providers, "zai")
		assert.Equal(t, "glm-5", f.DefaultModel)
	})

	t.Run("probe_result_is_cached", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{""}

		require.NoError(t, ProviderSetup(t.Context(), c))

		cache := llm.LoadUserCache()
		require.Contains(t, cache, "zai")
		require.Len(t, cache["zai"].Models, 1)
		assert.Equal(t, "glm-5", cache["zai"].Models[0].ID)
	})

	t.Run("registry_loads_the_written_provider", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{""}

		require.NoError(t, ProviderSetup(t.Context(), c))

		// the model itself is applied by the driver; the wizard leaves the
		// registry pointing at the written default
		assert.Equal(t, "zai/glm-5", c.Models().Active().Key())
	})

	t.Run("write_failure_is_reported", func(t *testing.T) {
		setupHome(t)
		c := newFakeConsole(t)
		stubProbe(t, stub, nil)
		c.picks = []fakePick{{result: pickName("zai")}, {result: 0}}
		c.inputs = []string{""}
		saveUserFile = func(llm.File) error { return errors.New("disk on fire") }
		t.Cleanup(func() { saveUserFile = llm.SaveUserFile })

		err := ProviderSetup(t.Context(), c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not write")
	})
}

func TestProviderSetupChoices(t *testing.T) {
	t.Parallel()

	t.Run("local_rows_come_first", func(t *testing.T) {
		for i, ch := range providerSetupChoices[:3] {
			assert.True(t, ch.local || ch.name == customProviderName, ch.name)
			assert.Equal(t, i, pickName(ch.name))
		}
		assert.False(t, providerSetupChoices[3].local)
	})

	t.Run("openrouter_follows_other", func(t *testing.T) {
		assert.Equal(t, 3, pickName("openrouter"))
		assert.Equal(t, 4, pickName("anthropic"))
	})

	t.Run("one_row_per_vendor", func(t *testing.T) {
		labels := make([]string, 0, len(providerSetupChoices))
		for _, ch := range providerSetupChoices {
			assert.NotContains(t, labels, ch.label)
			labels = append(labels, ch.label)
			assert.NotContains(t, ch.name, "-cn", ch.name)
		}
	})

	t.Run("groups_render_local_then_other_then_hosted", func(t *testing.T) {
		var order []string
		for _, ch := range providerSetupChoices {
			if len(order) == 0 || order[len(order)-1] != ch.group() {
				order = append(order, ch.group())
			}
		}
		assert.Equal(t, []string{"Local", "Other", "Hosted"}, order)
	})
}

func TestEnsureScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entered  string
		expected string
	}{
		{"host_port_gets_http", "192.168.1.5:1234", "http://192.168.1.5:1234"},
		{"localhost_gets_http", "localhost:8080", "http://localhost:8080"},
		{"http_is_kept", "http://h:1", "http://h:1"},
		{"https_is_kept", "https://h:1", "https://h:1"},
		{"empty_unchanged", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ensureScheme(tc.entered))
		})
	}
}

func TestInheritFlavorPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		defaultU string
		entered  string
		expected string
	}{
		{"bare_host_gets_the_path", "http://localhost:1234/v1", "http://192.168.1.5:1234", "http://192.168.1.5:1234/v1"},
		{"trailing_slash_gets_the_path", "http://localhost:1234/v1", "http://192.168.1.5:1234/", "http://192.168.1.5:1234/v1"},
		{"entered_path_is_kept", "http://localhost:1234/v1", "http://192.168.1.5:1234/api", "http://192.168.1.5:1234/api"},
		{"llamacpp_default_has_no_path", "http://localhost:8080", "http://192.168.1.5:8080", "http://192.168.1.5:8080"},
		{"bad_url_is_kept", "http://localhost:1234/v1", "http://192.168.1.5:port", "http://192.168.1.5:port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, inheritFlavorPath(tc.defaultU, tc.entered))
		})
	}
}
