package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckSecretPerms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		perm os.FileMode
		warn bool
	}{
		{"owner_only_ok", 0o600, false},
		{"owner_rw_x_ok", 0o700, false},
		{"group_readable_warns", 0o640, true},
		{"world_readable_warns", 0o604, true},
		{"world_writable_warns", 0o666, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "models.json")
			require.NoError(t, os.WriteFile(p, []byte("{}"), tc.perm))

			got := CheckSecretPerms(p)
			if tc.warn {
				assert.Contains(t, got, p)
			} else {
				assert.Empty(t, got)
			}
		})
	}
	t.Run("missing_file_is_silent", func(t *testing.T) {
		assert.Empty(t, CheckSecretPerms(filepath.Join(t.TempDir(), "absent.json")))
	})
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()

	t.Run("creates_with_perm", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.json")
		require.NoError(t, WriteFileAtomic(p, []byte(`{"a":1}`), SecretPerm))

		got, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.JSONEq(t, `{"a":1}`, string(got))

		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(SecretPerm), fi.Mode().Perm())
	})
	t.Run("overwrites_existing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.json")
		require.NoError(t, WriteFileAtomic(p, []byte("old"), SecretPerm))
		require.NoError(t, WriteFileAtomic(p, []byte("new"), SecretPerm))

		got, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.Equal(t, "new", string(got))
	})
	t.Run("creates_missing_dirs", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "nested", "deep", "cache.json")
		require.NoError(t, WriteFileAtomic(p, []byte("x"), SecretPerm))
		assert.FileExists(t, p)
	})
	t.Run("leaves_no_temp_files", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, WriteFileAtomic(filepath.Join(dir, "cache.json"), []byte("x"), SecretPerm))

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "cache.json", entries[0].Name())
	})
}
