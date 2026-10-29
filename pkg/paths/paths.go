// Package paths resolves ajent's directories on disk: the configuration home
// every other component derives its locations from, honoring AJENT_HOME.
// Client-side packages import it directly so they never depend on pkg/config.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// DirName is the configuration directory, relative to the user home.
	DirName = ".ajent"
	// EnvHome overrides the configuration directory entirely.
	EnvHome = "AJENT_HOME"
)

// DirPerm pins every directory ajent creates under its home to owner-only
// access.
const DirPerm = 0o700

// ErrNoHome is returned when neither AJENT_HOME nor a user home directory is set.
var ErrNoHome = errors.New("cannot locate the ajent configuration directory")

var (
	osEnv  = os.Getenv
	osHome = os.UserHomeDir
)

// Dir returns the ajent configuration directory, creating it when missing.
func Dir() (string, error) {
	dir, err := resolveDir(osEnv, osHome)
	if err != nil {
		return "", err
	} else if err = os.MkdirAll(dir, DirPerm); err != nil {
		return "", err
	}
	return dir, nil
}

// Home returns the resolved ajent configuration directory path without creating
// it. It honours AJENT_HOME and otherwise resolves <home>/.ajent, so callers that
// only probe for a file's existence avoid an empty-dir side effect.
func Home() (string, error) {
	return resolveDir(osEnv, osHome)
}

// SubDir returns name resolved under the configuration directory, creating it
// with owner-only permissions when missing. A group or world accessible mode is
// tightened in place and reported as a warning for callers to surface beside
// their other startup output; a clean or newly created directory reports empty.
func SubDir(name string) (string, string, error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	d := filepath.Join(dir, name)
	if err = os.MkdirAll(d, DirPerm); err != nil {
		return "", "", err
	}
	return d, tightenDir(d), nil
}

// tightenDir pins dir to DirPerm and describes what it found when the mode was
// wider. Mode bits are not meaningful on windows, where it always reports clean.
func tightenDir(dir string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return ""
	}
	mode := fi.Mode().Perm()
	if mode&0o077 == 0 {
		return ""
	}
	found := fmt.Sprintf("%s was group or world accessible (mode %o)", dir, mode)
	if err = os.Chmod(dir, DirPerm); err != nil {
		return found + "; chmod 700 it failed: " + err.Error()
	}
	return found + "; tightened to 0700"
}

// resolveDir returns the configuration directory for the given environment.
func resolveDir(env func(string) string, home func() (string, error)) (string, error) {
	if h := env(EnvHome); h != "" {
		return h, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	} else if h == "" {
		return "", ErrNoHome
	}
	return filepath.Join(h, DirName), nil
}
