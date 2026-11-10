package app

import "os"

// peerIdentity is a control socket peer's kernel-verified identity: the
// connect-time credential snapshot the operating system keeps for the accepted
// connection, never data the peer volunteered on the wire.
type peerIdentity struct {
	UID int
	PID int // zero when the platform does not report one (darwin)
}

// sameUser reports whether the peer connected under this process's effective
// uid: the "anything that can connect is already the user" claim, checked.
func (p peerIdentity) sameUser() bool { return p.UID == os.Geteuid() }
