//go:build unix

package session

import (
	"os"
	"syscall"
)

// lockWriter takes the transcript's exclusive advisory flock, blocking until a
// concurrent appender or repair lets go. Only EINTR is retried and any other
// error fails the append outright: no whole-line fallback below flock, since
// sessions assume a local filesystem. flock is advisory and per open file
// description, so it serializes cross-process writers without locking readers.
func lockWriter(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == syscall.EINTR {
			continue // interrupted by a signal; retry
		}
		return err
	}
}

// unlockWriter releases the advisory lock early; closing the fd releases it too.
func unlockWriter(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
