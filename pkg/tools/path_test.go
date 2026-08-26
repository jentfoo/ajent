package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	// several cases swap the package-global userHome, so this cannot run in parallel.

	t.Run("absolute_and_relative", func(t *testing.T) {
		cwd := t.TempDir()
		p := PathPolicy{Cwd: cwd}

		abs, err := p.Resolve(filepath.Join(cwd, "f.txt"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "f.txt"), abs)

		rel, err := p.Resolve("g.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "g.txt"), rel)
	})

	// no containment: any absolute path resolves even outside Cwd.
	t.Run("allows_absolute_outside_cwd", func(t *testing.T) {
		cwd := t.TempDir()
		outside := t.TempDir() // a dir outside Cwd to prove no containment

		p := PathPolicy{Cwd: cwd}
		abs, err := p.Resolve(filepath.Join(outside, "secret.txt"))
		require.NoError(t, err) // no containment; any absolute path resolves
		assert.Equal(t, filepath.Join(outside, "secret.txt"), abs)
	})

	// a symlink target outside Cwd is folded to canonical.
	t.Run("symlink_folded_to_canonical_path", func(t *testing.T) {
		cwd := t.TempDir()
		outside := t.TempDir() // symlink target outside Cwd is folded to canonical

		link := filepath.Join(cwd, "linked")
		require.NoError(t, os.Symlink(outside, link))

		p := PathPolicy{Cwd: cwd}
		resolved, err := p.Resolve(filepath.Join(link, "secret.txt"))
		require.NoError(t, err)
		// the symlink is folded so read/edit share one canonical tracker key
		assert.Equal(t, filepath.Join(outside, "secret.txt"), resolved)
	})

	// a new file in missing dirs still resolves to its cleaned absolute path.
	t.Run("new_file_in_missing_dir_still_works", func(t *testing.T) {
		cwd := t.TempDir()
		p := PathPolicy{Cwd: cwd}
		abs, err := p.Resolve(filepath.Join(cwd, "new", "dir", "file.txt"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "new", "dir", "file.txt"), abs) // cleaned absolute
	})

	t.Run("tilde_expands_home", func(t *testing.T) {
		home := t.TempDir()
		restore := setTestUserHome(home)
		t.Cleanup(restore)

		p := PathPolicy{Cwd: ""}
		abs, err := p.Resolve("~/f.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, "f.txt"), abs)
	})

	t.Run("bare_tilde_expands_home", func(t *testing.T) {
		home := t.TempDir()
		restore := setTestUserHome(home)
		t.Cleanup(restore)

		p := PathPolicy{Cwd: ""}
		abs, err := p.Resolve("~")
		require.NoError(t, err)
		assert.Equal(t, home, abs)
	})

	// ~ expands to home even when Cwd is set.
	t.Run("tilde_ignores_cwd", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir()
		restore := setTestUserHome(home)
		t.Cleanup(restore)

		p := PathPolicy{Cwd: cwd}
		abs, err := p.Resolve("~/f.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, "f.txt"), abs) // home wins over Cwd
	})

	// ~ is only special at the start of a path.
	t.Run("tilde_mid_path_not_expanded", func(t *testing.T) {
		cwd := t.TempDir()
		p := PathPolicy{Cwd: cwd}
		abs, err := p.Resolve("a~b.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "a~b.txt"), abs) // ~ only special at the start
	})

	// empty Cwd uses os.Getwd; a relative path joins onto it.
	t.Run("empty_cwd_falls_back_to_getwd", func(t *testing.T) {
		p := PathPolicy{}
		abs, err := p.Resolve("somefile.txt")
		require.NoError(t, err)
		cwd, _ := os.Getwd()
		assert.Equal(t, filepath.Join(cwd, "somefile.txt"), abs)
	})
}

// TestResolveTildeNotExpanded pins the paths that must stay relative rather
// than expand to home: relatives whose second char is `/`, and a leading ~ not
// followed by / (~user, ~~). Not parallel: swaps the package-global userHome.
func TestResolveTildeNotExpanded(t *testing.T) {
	home := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	cwd := t.TempDir()
	p := PathPolicy{Cwd: cwd}

	cases := []struct {
		name string
		rel  string
	}{
		{"relative_dotted_subdir", "./f.txt"},
		{"multi_segment_relative", "a/b/c.txt"},
		{"subdir_relative", "x/y"},
		{"user_prefixed_tilde", "~user/x"},
		{"double_tilde", "~~/y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			abs, err := p.Resolve(tc.rel)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(cwd, tc.rel), abs)
		})
	}
}

// setTestUserHome swaps userHome for the duration of a test and returns a
// restore func.
func setTestUserHome(home string) func() {
	orig := userHome
	userHome = func() (string, error) { return home, nil }
	return func() { userHome = orig }
}
