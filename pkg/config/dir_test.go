package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHomeDoesNotCreateDir(t *testing.T) {
	// t.Setenv below forbids t.Parallel: env vars are process-global
	root := filepath.Join(t.TempDir(), "nested", "ajent")
	t.Setenv(EnvHome, root)

	got, err := Home()
	require.NoError(t, err)
	assert.Equal(t, root, got)
	_, statErr := os.Stat(root)
	assert.ErrorIs(t, statErr, os.ErrNotExist) // probing must not materialise the dir
}

func TestUserPath(t *testing.T) {
	t.Run("creates_missing_directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "nested", "ajent")
		t.Setenv(EnvHome, root)

		got, err := UserPath("models.json")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "models.json"), got)
		assert.DirExists(t, root)
	})

	t.Run("existing_file_blocks_creation", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "ajent")
		require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
		t.Setenv(EnvHome, blocked)

		_, err := UserPath("models.json")
		require.Error(t, err)
	})
}
