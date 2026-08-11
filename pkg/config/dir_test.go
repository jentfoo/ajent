package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDir(t *testing.T) {
	t.Parallel()

	homeOK := func(p string) func() (string, error) {
		return func() (string, error) { return p, nil }
	}
	noEnv := func(string) string { return "" }

	t.Run("env_override_wins", func(t *testing.T) {
		env := func(k string) string {
			if k == EnvHome {
				return "/custom/ajent"
			}
			return ""
		}
		dir, err := resolveDir(env, homeOK("/home/u"))
		require.NoError(t, err)
		assert.Equal(t, "/custom/ajent", dir)
	})
	t.Run("falls_back_to_home", func(t *testing.T) {
		dir, err := resolveDir(noEnv, homeOK("/home/u"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/home/u", DirName), dir)
	})
	t.Run("home_error_propagates", func(t *testing.T) {
		want := errors.New("boom")
		_, err := resolveDir(noEnv, func() (string, error) { return "", want })
		assert.ErrorIs(t, err, want)
	})
	t.Run("empty_home_errors", func(t *testing.T) {
		_, err := resolveDir(noEnv, homeOK(""))
		assert.ErrorIs(t, err, ErrNoHome)
	})
}

func TestUserPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)

	p, err := UserPath("models.json")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "models.json"), p)
}

func TestDir(t *testing.T) {
	t.Run("creates_missing_directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "nested", "ajent")
		t.Setenv(EnvHome, root)

		got, err := Dir()
		require.NoError(t, err)
		assert.Equal(t, root, got)
		assert.DirExists(t, root)
	})
	t.Run("existing_file_blocks_creation", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "ajent")
		require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
		t.Setenv(EnvHome, blocked)

		_, err := Dir()
		require.Error(t, err)

		_, err = UserPath("models.json")
		require.Error(t, err)
	})
}
