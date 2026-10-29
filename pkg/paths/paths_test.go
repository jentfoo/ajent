package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	})
}

func TestSubDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful")
	}

	root := t.TempDir()
	t.Setenv(EnvHome, root)

	t.Run("creates_missing_directory", func(t *testing.T) {
		dir, warn, err := SubDir("fresh")
		require.NoError(t, err)
		assert.Empty(t, warn)
		assert.Equal(t, filepath.Join(root, "fresh"), dir)
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(DirPerm), fi.Mode().Perm())
	})

	t.Run("tightens_wide_directory", func(t *testing.T) {
		wide := filepath.Join(root, "wide")
		require.NoError(t, os.Mkdir(wide, 0o750))

		dir, warn, err := SubDir("wide")
		require.NoError(t, err)
		assert.Equal(t, wide, dir)
		assert.Contains(t, warn, "tightened to 0700")
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(DirPerm), fi.Mode().Perm())
	})

	t.Run("clean_directory_stays_silent", func(t *testing.T) {
		_, warn, err := SubDir("fresh")
		require.NoError(t, err)
		assert.Empty(t, warn)
	})
}

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
