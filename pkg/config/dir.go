package config

import (
	"os"
	"path/filepath"

	"github.com/jentfoo/ajent/pkg/paths"
)

const (
	// DirName is the configuration directory, relative to the user home.
	DirName = paths.DirName
	// EnvHome overrides the configuration directory entirely.
	EnvHome = paths.EnvHome

	// CacheDirName holds disposable caches under the configuration directory.
	CacheDirName = "cache"
)

// ErrNoHome is returned when neither AJENT_HOME nor a user home directory is set.
var ErrNoHome = paths.ErrNoHome

// UserPath returns name resolved inside the configuration directory, creating
// it when missing. Use Home to avoid that side effect.
func UserPath(name string) (string, error) {
	dir, err := paths.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Home returns the resolved ajent configuration directory path without creating
// it. It honours AJENT_HOME and otherwise resolves <home>/.ajent, so callers that
// only probe for a file's existence avoid an empty-dir side effect.
func Home() (string, error) {
	return paths.Home()
}

// CachePath returns name resolved inside the configuration directory's cache
// subdirectory, creating it when missing.
func CachePath(name string) (string, error) {
	dir, err := paths.Dir()
	if err != nil {
		return "", err
	}
	cd := filepath.Join(dir, CacheDirName)
	if err = os.MkdirAll(cd, paths.DirPerm); err != nil {
		return "", err
	}
	return filepath.Join(cd, name), nil
}

// Cwd returns the current working directory, or "." when it is unavailable.
func Cwd() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "."
	}
	return cwd
}

// ProjectDir returns the configuration directory for a workspace.
func ProjectDir(workspace string) string {
	return filepath.Join(workspace, DirName)
}
