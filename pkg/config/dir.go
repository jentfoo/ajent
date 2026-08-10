// Package config resolves the ajent configuration directory and provides the
// file primitives the rest of the system loads its configuration through. It
// holds no domain types and must never import pkg/llm.
package config

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	// DirName is the configuration directory, relative to the user home.
	DirName = ".ajent"
	// EnvHome overrides the configuration directory entirely.
	EnvHome = "AJENT_HOME"

	dirPerm = 0o700
)

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
	} else if err = os.MkdirAll(dir, dirPerm); err != nil {
		return "", err
	}
	return dir, nil
}

// UserPath returns name resolved inside the configuration directory.
func UserPath(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// ProjectDir returns the configuration directory for a workspace.
func ProjectDir(workspace string) string {
	return filepath.Join(workspace, DirName)
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
