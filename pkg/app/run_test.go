package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
)

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	assert.False(t, isTerminal(r)) // a pipe routes the run to the headless path
	assert.False(t, isTerminal(w))

	f, err := os.CreateTemp(t.TempDir(), "f")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	assert.False(t, isTerminal(f)) // a redirected file is not a terminal either
}

// withModelsHome writes a two-provider catalogue into a private AJENT_HOME so
// registryFor reads it instead of the developer's models.json.
func withModelsHome(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	const models = `{
	  "providers": {
	    "alpha": {"models": [{"id": "a1"}, {"id": "a2"}]},
	    "beta": {"models": [{"id": "b1"}]}
	  }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(home, llm.ModelsFileName), []byte(models), 0o600))
}

// flagSet mirrors run.go's flag layer: -m injects into config key model, which
// outranks every file layer.
func flagSet(t *testing.T, model string) *config.Set {
	t.Helper()

	flagLayer := config.Layer{Name: "flag"}
	if model != "" {
		data, err := config.SetKey(flagLayer.Data, "model", model)
		require.NoError(t, err)
		flagLayer.Data = data
	}
	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
		Flags:     flagLayer,
	})
	require.NoError(t, err)
	return set
}

func TestRegistryForModelFlag(t *testing.T) {
	// the -m model resolves exactly once, inside the registry build: activateDefault
	// owns both activation and the unresolvable-name warning
	withModelsHome(t) // not parallel: AJENT_HOME is process state

	t.Run("valid_model_activates", func(t *testing.T) {
		reg, warnings, err := registryFor(flagSet(t, "beta/b1"))
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Equal(t, "beta/b1", reg.Active().Key())
	})

	t.Run("garbage_warns_once_then_falls_back", func(t *testing.T) {
		reg, warnings, err := registryFor(flagSet(t, "no-such-model"))
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		assert.Equal(t, "defaultModel no-such-model not found", warnings[0])
		assert.Equal(t, "alpha/a1", reg.Active().Key()) // the first model in the catalogue
	})

	t.Run("no_flag_takes_the_first_model", func(t *testing.T) {
		reg, warnings, err := registryFor(flagSet(t, ""))
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Equal(t, "alpha/a1", reg.Active().Key())
	})
}
