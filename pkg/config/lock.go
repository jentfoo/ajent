package config

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// lockWait bounds the poll for a sibling process's advisory lock before saving
// proceeds unlocked. Blocking is ruled out: command handlers run inline on the
// prompt pump, so a stalled holder (SIGSTOP, debugger, slow fsync) must never
// freeze another session's dispatch.
var lockWait = 3 * time.Second

const lockPoll = 10 * time.Millisecond

// errLockContended reports a sibling holds the save lock and waiting may help.
// Any other lock error means locking is unavailable here; saving degrades unlocked.
var errLockContended = errors.New("config: advisory lock held by another process")

// LockSave takes the cross-process exclusion around one file's read-modify-write
// save: an exclusive flock on a sidecar "<name>.lock" beside it. The sidecar is
// required because WriteFileAtomic renames a fresh inode over the target, so a
// lock held on the file itself excludes no one.
//
// Acquisition is bounded and best-effort: past the wait deadline, or wherever
// locking is unavailable (off-unix platforms, exotic filesystems), saving
// proceeds unlocked. The returned release func is never nil and safe to defer;
// failures degrade silently because a save may never hang or error over its lock.
func LockSave(target string) (release func()) {
	noop := func() {}
	sidecar := target + ".lock"
	if err := os.MkdirAll(filepath.Dir(sidecar), dirPerm); err != nil {
		return noop
	}
	f, err := os.OpenFile(sidecar, os.O_CREATE|os.O_RDWR, SecretPerm)
	if err != nil {
		return noop // exotic or network filesystem: unlocked is the norm there
	}

	if !lockSidecar(f) {
		_ = f.Close()
		return noop
	}
	return func() { _ = f.Close() } // closing releases the flock
}

// lockSidecar polls until the exclusive lock is held. It reports false once past
// the wait deadline (a sibling holds it) or when locking fails outright.
func lockSidecar(f *os.File) bool {
	deadline := time.Now().Add(lockWait)
	for {
		err := lockExclusive(f)
		if err == nil {
			return true
		} else if !errors.Is(err, errLockContended) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}
