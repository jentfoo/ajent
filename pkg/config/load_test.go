package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadEnv returns an env lookup rooted at vars plus a fallback to os.Getenv.
func loadEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadLayeredPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	ws := filepath.Join(home, "proj")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	writeConfig(t, userPathFor(t), `{"model":"user-model","reasoning":{"level":"low"}}`)
	writeConfig(t, filepath.Join(ProjectDir(ws), ConfigFileName), `{"model":"project-model","compaction":{"threshold":0.3}}`)

	s, warns, err := Load(Options{Workspace: ws})
	require.NoError(t, err)
	assert.Empty(t, warns)

	st := s.Settings()
	assert.Equal(t, "project-model", st.Model) // project wins over user
	assert.Equal(t, "low", st.Reasoning.Level) // only user sets it

	_, src, ok := s.Explain("model")
	require.True(t, ok)
	assert.Equal(t, "project", src)

	// Explain reports the merged value and its source layer for a nested key
	v, src2, found := s.Explain("reasoning.level")
	require.True(t, found)
	assert.Equal(t, "user", src2) // only the user file sets it
	var lvl string
	require.NoError(t, json.Unmarshal(v, &lvl))
	assert.Equal(t, "low", lvl)
}

func TestLoadEnvAndFlagOutrankFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)

	s, _, err := Load(Options{
		Workspace: ".",
		Env:       loadEnv(map[string]string{"AJENT_REASONING_LEVEL": "high"}),
		Flags:     mustFlagLayer(t, map[string]any{"model": "flag-model"}),
	})
	require.NoError(t, err)

	st := s.Settings()
	assert.Equal(t, "flag-model", st.Model) // flag beats env-less model
	assert.Equal(t, "high", st.Reasoning.Level)
}

func TestLoadProjectAPIKeyStrippedAndWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	ws := filepath.Join(home, "proj")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	writeConfig(t, filepath.Join(ProjectDir(ws), ConfigFileName),
		`{"providers":{"anthropic":{"apiKey":"SECRET","baseUrl":"x"}}}`)

	s, warns, err := Load(Options{Workspace: ws})
	require.NoError(t, err)
	assert.NotEmpty(t, warns)
	assert.Contains(t, strings.ToLower(strings.Join(warns, " ")), "ignored apikey")
	// the literal secret never reaches settings; unrelated provider fields survive
	st := s.Settings()
	require.NotEmpty(t, st.Providers)
	assert.NotContains(t, string(st.Providers), "SECRET")
}

func TestLoadUserSecretPermWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on windows")
	}

	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	p := userPathFor(t)
	writeConfig(t, p, `{"providers":{"a":{"apiKey":"x"}}}`)
	require.NoError(t, os.Chmod(p, 0o644))

	s, warns, err := Load(Options{Workspace: "."})
	require.NoError(t, err)
	assert.NotNil(t, s)
	assert.Contains(t, strings.ToLower(strings.Join(warns, " ")), "readable by other users")
}

func TestSaveWritesAndReResolves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	s, _, err := Load(Options{Workspace: "."})
	require.NoError(t, err)

	warns, err := s.Save("user", "reasoning.level", "high")
	require.NoError(t, err)
	assert.Empty(t, warns)

	st := s.Settings()
	assert.Equal(t, "high", st.Reasoning.Level)
	_, src, _ := s.Explain("reasoning.level")
	assert.Equal(t, "user", src)

	// the file on disk carries it too
	var raw map[string]any
	b, err := os.ReadFile(s.userPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Equal(t, "high", raw["reasoning"].(map[string]any)["level"])
}

func TestSaveUnknownLayerErrors(t *testing.T) {
	t.Parallel()

	s, _, err := Load(Options{Workspace: "."})
	require.NoError(t, err)
	_, err = s.Save("bogus", "model", "x")
	assert.Error(t, err)
}

// helpers ----------------------------------------------------------------

func userPathFor(t *testing.T) string {
	t.Helper()
	p, err := UserPath(ConfigFileName)
	if err != nil {
		return filepath.Join(os.Getenv("AJENT_HOME"), ConfigFileName)
	}
	return p
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// mustFlagLayer builds a flag layer from key/value pairs.
func mustFlagLayer(t *testing.T, kvs map[string]any) Layer {
	t.Helper()
	data := []byte("{}")
	var err error
	for k, v := range kvs {
		data, err = SetKey(data, k, v)
		require.NoError(t, err)
	}
	return Layer{Name: "flag", Data: data}
}

func TestLoadMissingFilesAreNotErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	s, warns, err := Load(Options{Workspace: filepath.Join(home, "empty")})
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.NotNil(t, s.Settings())
}
