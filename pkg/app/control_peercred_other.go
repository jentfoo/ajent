//go:build !linux && !darwin

package app

import (
	"errors"
	"net"
)

// peerCredentials cannot attest identity on this platform, so every control
// connection is refused: the same-user model requires kernel-verified
// credentials, and failing open would widen it silently.
func peerCredentials(nc net.Conn) (peerIdentity, error) {
	return peerIdentity{}, errors.New("peer credential verification is unavailable on this platform")
}
