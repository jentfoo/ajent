package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAbsoluteAndRelative(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	p := PathPolicy{Cwd: cwd}

	abs, err := p.Resolve(filepath.Join(cwd, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "f.txt"), abs)

	rel, err := p.Resolve("g.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "g.txt"), rel)
}

func TestResolveAllowsAbsoluteOutsideCwd(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	outside := t.TempDir() // a dir outside Cwd to prove no containment

	p := PathPolicy{Cwd: cwd}
	abs, err := p.Resolve(filepath.Join(outside, "secret.txt"))
	require.NoError(t, err) // no containment; any absolute path resolves
	assert.Equal(t, filepath.Join(outside, "secret.txt"), abs)
}

func TestResolveSymlinkFoldedToCanonicalPath(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	outside := t.TempDir() // symlink target outside Cwd is folded to canonical

	link := filepath.Join(cwd, "linked")
	require.NoError(t, os.Symlink(outside, link))

	p := PathPolicy{Cwd: cwd}
	resolved, err := p.Resolve(filepath.Join(link, "secret.txt"))
	require.NoError(t, err)
	// the symlink is folded so read/edit share one canonical tracker key
	assert.Equal(t, filepath.Join(outside, "secret.txt"), resolved)
}

func TestResolveNewFileInMissingDirStillWorks(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	p := PathPolicy{Cwd: cwd}
	abs, err := p.Resolve(filepath.Join(cwd, "new", "dir", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "new", "dir", "file.txt"), abs) // cleaned absolute
}

func TestResolveTildeExpandsHome(t *testing.T) {
	home := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	p := PathPolicy{Cwd: ""}
	abs, err := p.Resolve("~/f.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "f.txt"), abs)
}

func TestResolveBareTildeExpandsHome(t *testing.T) {
	home := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	p := PathPolicy{Cwd: ""}
	abs, err := p.Resolve("~")
	require.NoError(t, err)
	assert.Equal(t, home, abs)
}

func TestResolveTildeIgnoresCwd(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	p := PathPolicy{Cwd: cwd}
	abs, err := p.Resolve("~/f.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "f.txt"), abs) // home wins over Cwd
}

func TestResolveTildeMidPathNotExpanded(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	p := PathPolicy{Cwd: cwd}
	abs, err := p.Resolve("a~b.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "a~b.txt"), abs) // ~ only special at the start
}

func TestResolveRelativeSubdirNotTildeExpanded(t *testing.T) {
	t.Parallel()

	// a path whose second char is `/` but first is not `~` must resolve from Cwd,
	// never as a home expansion: ./ and multi-segment relatives both hit this.
	cwd := t.TempDir()
	home := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	for _, rel := range []string{"./f.txt", "a/b/c.txt", "x/y"} {
		abs, err := (PathPolicy{Cwd: cwd}).Resolve(rel)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, rel), abs, "relative %q must not become a ~ path", rel)
	}
}

func TestResolveLeadingTildeNoSlashNotExpanded(t *testing.T) {
	t.Parallel()

	// a leading ~ not followed by / (~user, ~~) is left literal; only ~ or ~/
	// expand to home.
	cwd := t.TempDir()
	home := t.TempDir()
	restore := setTestUserHome(home)
	t.Cleanup(restore)

	for _, rel := range []string{"~user/x", "~~/y"} {
		abs, err := (PathPolicy{Cwd: cwd}).Resolve(rel)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, rel), abs, "leading ~ without slash %q must stay literal", rel)
	}
}

// setTestUserHome swaps userHome for the duration of a test and returns a
// restore func.
func setTestUserHome(home string) func() {
	orig := userHome
	userHome = func() (string, error) { return home, nil }
	return func() { userHome = orig }
}

func TestResolveEmptyCwdFallsBackToGetwd(t *testing.T) {
	t.Parallel()

	// empty Cwd uses os.Getwd; a relative path should join onto it
	p := PathPolicy{}
	abs, err := p.Resolve("somefile.txt")
	require.NoError(t, err)
	cwd, _ := os.Getwd()
	assert.Equal(t, filepath.Join(cwd, "somefile.txt"), abs)
}
