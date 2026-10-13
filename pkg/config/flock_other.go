//go:build !unix

package config

import (
	"errors"
	"os"
)

// flockSupported gates lockfile creation: where flock does not exist the file
// would be permanent clutter that locks nothing.
const flockSupported = false

// errNoAdvisory degrades saves to unlocked where flock does not exist,
// mirroring the syncDir divergence in file.go.
var errNoAdvisory = errors.New("config: advisory locking unavailable on this platform")

func lockExclusive(*os.File) error {
	return errNoAdvisory
}
