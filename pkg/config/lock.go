package config

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// lockWait bounds the poll for a sibling's advisory lock before saving proceeds
// unlocked: a healthy sibling's write takes milliseconds, a stuck one never pays.
var lockWait = 2 * time.Second

const lockPoll = 10 * time.Millisecond

// lockFileName is the one save lock, shared by every locked writer: both targets
// live in the config directory and each write takes milliseconds.
const lockFileName = "saves.lock"

// errLockContended reports a sibling holds the save lock and waiting may help.
// Any other lock error means locking is unavailable here; saving degrades unlocked.
var errLockContended = errors.New("config: advisory lock held by another process")

// LockSave holds the cross-process exclusion around one read-modify-write save of
// ajent-owned state (user config layer, models.json): an exclusive, bounded,
// best-effort flock on saves.lock in the config directory. The returned release
// func is never nil and safe to defer; every failure mode degrades silently.
func LockSave() (release func()) {
	noop := func() {}
	if !flockSupported {
		return noop
	}
	dir, err := Dir()
	if err != nil {
		return noop
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, SecretPerm)
	if err != nil {
		return noop // exotic or network filesystem: unlocked is the norm there
	}

	if !acquireLock(f) {
		_ = f.Close()
		return noop
	}
	return func() { _ = f.Close() } // closing releases the flock
}

// acquireLock polls until the exclusive lock is held. It reports false once past
// the wait deadline (a sibling holds it) or when locking fails outright.
func acquireLock(f *os.File) bool {
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
