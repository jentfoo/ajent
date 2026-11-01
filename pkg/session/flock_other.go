//go:build !unix

package session

import "os"

// lockWriter is the no-flock fallback: O_APPEND whole-line writes carry the
// cross-writer guarantee there, and repair races are accepted as they are today.
func lockWriter(*os.File) error { return nil }

// unlockWriter mirrors lockWriter's platform split.
func unlockWriter(*os.File) {}
