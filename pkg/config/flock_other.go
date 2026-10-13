//go:build !unix

package config

import (
	"errors"
	"os"
)

// errNoAdvisory degrades saves to unlocked where flock does not exist,
// mirroring the syncDir divergence in file.go.
var errNoAdvisory = errors.New("config: advisory locking unavailable on this platform")

func lockExclusive(*os.File) error {
	return errNoAdvisory
}
