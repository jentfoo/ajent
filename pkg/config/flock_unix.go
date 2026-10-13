//go:build unix

package config

import (
	"errors"
	"os"
	"syscall"
)

// flockSupported gates lockfile creation: where flock does not exist the file
// would be permanent clutter that locks nothing.
const flockSupported = true

// lockExclusive takes an exclusive advisory lock without blocking, retrying only
// EINTR. A held lock reports errLockContended; any other error means the platform
// or filesystem cannot lock here and saving must degrade unlocked.
func lockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == syscall.EINTR {
			continue // interrupted by a signal; retry
		} else if errors.Is(err, syscall.EAGAIN) { // EWOULDBLOCK is EAGAIN on every unix
			return errLockContended
		}
		return err
	}
}
