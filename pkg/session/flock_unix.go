//go:build unix

package session

import (
	"os"
	"syscall"
)

// lockWriter takes the transcript's exclusive advisory flock, blocking until a
// concurrent appender or repair lets go. Only EINTR is retried; any other error
// returns as is, since a filesystem without flock degrades to whole-line O_APPEND
// writes rather than failing appends. flock is advisory and per open file
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
