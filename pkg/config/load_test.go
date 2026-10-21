package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadEnv returns an env lookup rooted at vars plus a fallback to os.Getenv.
func loadEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoad(t *testing.T) {
	t.Run("layered_precedence", func(t *testing.T) {
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
	})

	t.Run("env_and_flag_outrank_files", func(t *testing.T) {
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
	})

	t.Run("missing_files_are_not_errors", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		s, warns, err := Load(Options{Workspace: filepath.Join(home, "empty")})
		require.NoError(t, err)
		assert.Empty(t, warns)
		assert.NotNil(t, s.Settings())
	})

	t.Run("empty_file_loads_as_absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		ws := filepath.Join(home, "proj")
		require.NoError(t, os.MkdirAll(ws, 0o755))

		writeConfig(t, userPathFor(t), "// no settings yet\n")            // comment-only
		writeConfig(t, filepath.Join(ProjectDir(ws), ConfigFileName), "") // zero bytes, like touch
		writeConfig(t, filepath.Join(ProjectDir(ws), LocalConfigFileName), " \t\n ")

		s, warns, err := Load(Options{Workspace: ws})
		require.NoError(t, err)
		assert.Empty(t, warns)

		// defaults resolve as if every file were missing
		st := s.Settings()
		assert.Equal(t, "medium", st.Reasoning.Level)
		assert.Equal(t, "default", s.Source("reasoning.level"))
	})

	t.Run("empty_file_beside_valid_layer", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		ws := filepath.Join(home, "proj")
		require.NoError(t, os.MkdirAll(ws, 0o755))

		writeConfig(t, userPathFor(t), "")
		writeConfig(t, filepath.Join(ProjectDir(ws), ConfigFileName), `{"model":"project-model"}`)

		s, warns, err := Load(Options{Workspace: ws})
		require.NoError(t, err)
		assert.Empty(t, warns)
		assert.Equal(t, "project-model", s.Settings().Model)
	})

	t.Run("stray_documents_name_the_file", func(t *testing.T) {
		for _, doc := range []string{"null", `[1]`, `"x"`, "123"} {
			t.Setenv("AJENT_HOME", t.TempDir())
			path := userPathFor(t)
			writeConfig(t, path, doc)

			s, warns, err := Load(Options{Workspace: t.TempDir()})
			assert.Nil(t, s) // startup aborts instead of merging a stray word
			assert.Empty(t, warns)
			require.Error(t, err)
			assert.Contains(t, err.Error(), path)
			assert.Contains(t, err.Error(), "expected a JSON object")
		}
	})

	t.Run("stray_document_names_the_line", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), "// tuned by hand\nnull")

		_, _, err := Load(Options{Workspace: t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), ":2:1: expected a JSON object at the top level, found null")
	})

	t.Run("type_error_warns", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), `{"model":"keep","agent":{"maxSteps":"ten"}}`)

		s, warns, err := Load(Options{Workspace: filepath.Join(home, "proj")})
		require.NoError(t, err)
		assert.Len(t, warns, 1)
		assert.Contains(t, warns[0], `wrong type for key "agent.maxSteps": got a string, want an integer`)

		// the wrong-typed key is dropped but its siblings still apply
		assert.Equal(t, "keep", s.Settings().Model)
	})

	t.Run("odd_case_keys_normalize", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), `{"UI":{"Theme":"dark"}}`)

		s, warns, err := Load(Options{Workspace: filepath.Join(home, "proj")})
		require.NoError(t, err)
		require.NotEmpty(t, warns)
		assert.Contains(t, warns[0], `normalized key "UI" to "ui"`)

		// typed decode and provenance now agree on one spelling
		st := s.Settings()
		assert.Equal(t, "dark", st.UI.Theme)
		assert.Equal(t, "user", s.Source("ui.theme"))
	})
}

func TestSetConcurrentAccess(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())
	s, _, err := Load(Options{Workspace: t.TempDir()})
	require.NoError(t, err)

	// settings read while session overrides write, as the pump and key handlers do
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Settings().Model
			}
		}()
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				assert.NoError(t, s.SetSession("model", fmt.Sprintf("p/m%d", i)))
			}
		}(i)
	}
	wg.Wait()

	require.NoError(t, s.SetSession("model", "p/final"))
	assert.Equal(t, "p/final", s.Settings().Model)
}

func TestSeedSession(t *testing.T) {
	t.Run("seeds_sorted_keys", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)

		overrides := map[string]json.RawMessage{
			"reasoning.budget": json.RawMessage(`512`),
			"reasoning.hide":   json.RawMessage(`true`),
			"reasoning.level":  json.RawMessage(`"high"`),
			"model":            json.RawMessage(`"p/m"`),
		}
		s.SeedSession(overrides)
		got := s.resolve().Bytes()

		// a second Set folds the same overrides; the merged bytes must match byte for byte
		s2, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)
		s2.SeedSession(overrides)
		assert.Equal(t, string(got), string(s2.resolve().Bytes()))

		st := s.Settings()
		assert.Equal(t, "p/m", st.Model)
		assert.Equal(t, 512, st.Reasoning.Budget)
		assert.True(t, st.Reasoning.Hide)
		assert.Equal(t, "high", st.Reasoning.Level)
	})

	t.Run("skips_empty_overrides", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)

		s.SeedSession(map[string]json.RawMessage{"model": json.RawMessage(`"p/m"`), "agent.maxSteps": {}})
		assert.Equal(t, "p/m", s.Settings().Model)

		_, _, ok := s.Explain("agent.maxSteps")
		assert.False(t, ok)
	})

	t.Run("null_removes_recorded_reset", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)
		require.NoError(t, s.SetSession("model", "stale"))

		s.SeedSession(map[string]json.RawMessage{
			"model":           json.RawMessage(`null`), // a reset beats the older live override
			"reasoning.level": json.RawMessage(`"high"`),
		})
		_, _, ok := s.Explain("model")
		assert.False(t, ok)
		assert.Equal(t, "high", s.Settings().Reasoning.Level)
	})

	t.Run("corrupt_override_warns_and_skips", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)
		require.NoError(t, s.SetSession("model", "keep"))

		warns := s.SeedSession(map[string]json.RawMessage{
			"agent.maxSteps":  json.RawMessage(`null`), // a recorded reset still applies
			"model":           json.RawMessage(`{"oops"`),
			"reasoning.level": json.RawMessage(`"high"`),
		})
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], `"model"`)

		st := s.Settings()
		assert.Equal(t, "keep", st.Model)           // the bad key is skipped, not wiped
		assert.Equal(t, "high", st.Reasoning.Level) // siblings still seed
	})
}

func TestSave(t *testing.T) {
	t.Run("writes_and_reresolves", func(t *testing.T) {
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
	})

	t.Run("user_save_takes_shared_lockfile", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		_, err = s.Save("user", "model", "p/m")
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(home, lockFileName))
		_, err = os.Stat(s.userPath + ".lock")
		assert.True(t, os.IsNotExist(err)) // no per-file sidecar beside the target
	})

	t.Run("project_save_skips_sidecar", func(t *testing.T) {
		home := t.TempDir()
		ws := filepath.Join(home, "proj")
		t.Setenv("AJENT_HOME", home)
		s, _, err := Load(Options{Workspace: ws})
		require.NoError(t, err)

		_, err = s.Save("project", "model", "p/m")
		require.NoError(t, err)

		_, err = os.Stat(filepath.Join(ProjectDir(ws), ConfigFileName) + ".lock")
		assert.True(t, os.IsNotExist(err)) // no lockfile litter in the user's repo
	})

	t.Run("save_proceeds_when_sibling_holds_lock", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		// a sibling process holding the lock degrades the save to unlocked
		held, err := os.OpenFile(filepath.Join(home, lockFileName), os.O_CREATE|os.O_RDWR, SecretPerm)
		require.NoError(t, err)
		t.Cleanup(func() { _ = held.Close() })
		require.NoError(t, lockExclusive(held))
		shortenWait(t)

		warns, err := s.Save("user", "reasoning.level", "high")
		require.NoError(t, err)
		assert.Empty(t, warns)
		assert.Equal(t, "high", s.Settings().Reasoning.Level)
	})

	t.Run("unknown_layer_errors", func(t *testing.T) {
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)
		_, err = s.Save("bogus", "model", "x")
		assert.Error(t, err)
	})

	t.Run("save_normalizes_odd_case", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), `{"MODEL":"keep","UI":{"Theme":"dark"}}`)
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		warns, err := s.Save("user", "reasoning.level", "high")
		require.NoError(t, err)
		assert.Empty(t, warns) // load already reported the renames; saving stays silent

		b, err := os.ReadFile(s.userPath)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(b, &raw))
		assert.NotContains(t, string(b), "MODEL") // no twin keys survive the save
		assert.NotContains(t, string(b), `"UI"`)
		assert.Equal(t, "keep", raw["model"])
		assert.Equal(t, "dark", raw["ui"].(map[string]any)["theme"])
		assert.Equal(t, "high", raw["reasoning"].(map[string]any)["level"])
	})

	t.Run("commented_file_saves", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), `{
  // user preference
  "model": "old",
}`)
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		warns, err := s.Save("user", "reasoning.level", "high")
		require.NoError(t, err)
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], "has comments") // comments are dropped but save succeeds

		st := s.Settings()
		assert.Equal(t, "high", st.Reasoning.Level)
	})

	t.Run("hand_edited_trailing_bytes_refuse_save", func(t *testing.T) {
		// trailing bytes only exist via hand edits after startup because Load rejects them
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		userFile := userPathFor(t)
		writeConfig(t, userFile, `{"model":"keep"}`)
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		junked := "{\"model\":\"keep\"} oops\n"
		require.NoError(t, os.WriteFile(userFile, []byte(junked), 0o600))

		_, err = s.Save("user", "reasoning.level", "high")
		require.Error(t, err)

		b, err := os.ReadFile(userFile)
		require.NoError(t, err)
		assert.Equal(t, junked, string(b)) // the hand edit is left untouched
	})

	t.Run("trailing_comma_only_file_saves_silently", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		writeConfig(t, userPathFor(t), "{\"model\": \"old\",\n}")
		s, _, err := Load(Options{Workspace: "."})
		require.NoError(t, err)

		warns, err := s.Save("user", "reasoning.level", "high")
		require.NoError(t, err)
		assert.Empty(t, warns) // a trailing comma is fixed, not content lost

		b, err := os.ReadFile(s.userPath)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(b, &raw))
		assert.Equal(t, "old", raw["model"])
		assert.Equal(t, "high", raw["reasoning"].(map[string]any)["level"])
	})
}

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

func TestClearSession(t *testing.T) {
	newSet := func(tb testing.TB) *Set {
		tb.Helper()
		t.Setenv("AJENT_HOME", tb.TempDir())
		s, _, err := Load(Options{Workspace: tb.TempDir()})
		require.NoError(tb, err)
		return s
	}

	t.Run("falls_back_to_lower_layer", func(t *testing.T) {
		s := newSet(t)
		require.NoError(t, s.SetSession("model", "session-model"))
		assert.Equal(t, "session-model", s.Settings().Model)

		require.NoError(t, s.ClearSession("model"))
		assert.Empty(t, s.Settings().Model) // no layer below declares model either
		_, _, ok := s.Explain("model")
		assert.False(t, ok)
	})

	t.Run("clears_whole_subtree", func(t *testing.T) {
		s := newSet(t)
		require.NoError(t, s.SetSession("reasoning.level", "high"))
		require.NoError(t, s.SetSession("reasoning.retain", "all"))

		require.NoError(t, s.ClearSession("reasoning"))
		// the compiled-in default applies again, like a session never overrode it
		st := s.Settings()
		assert.Equal(t, "medium", st.Reasoning.Level)
		_, src, _ := s.Explain("reasoning.level")
		assert.Equal(t, "default", src)
	})

	t.Run("unknown_key_is_harmless", func(t *testing.T) {
		s := newSet(t)
		require.NoError(t, s.SetSession("model", "session-model"))
		require.NoError(t, s.ClearSession("agent.maxSteps", "missing.key"))
		assert.Equal(t, "session-model", s.Settings().Model)
	})
}

func TestSessionOverrides(t *testing.T) {
	t.Run("empty_when_no_overrides", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)
		assert.Empty(t, s.SessionOverrides())
	})

	t.Run("lists_sorted_leaves", func(t *testing.T) {
		t.Setenv("AJENT_HOME", t.TempDir())
		s, _, err := Load(Options{Workspace: t.TempDir()})
		require.NoError(t, err)

		require.NoError(t, s.SetSession("tools.limits.bash.lines", 10))
		require.NoError(t, s.SetSession("model", "p/m"))
		require.NoError(t, s.SetSession("reasoning.level", "high"))

		assert.Equal(t, []string{"model", "reasoning.level", "tools.limits.bash.lines"},
			s.SessionOverrides())

		require.NoError(t, s.ClearSession("reasoning.level"))
		assert.Equal(t, []string{"model", "tools.limits.bash.lines"}, s.SessionOverrides())
	})
}
