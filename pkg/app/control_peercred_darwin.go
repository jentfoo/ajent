//go:build darwin

package app

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerCredentials reads the kernel's credential snapshot for one accepted unix
// connection. darwin attests the effective uid only; no peer pid is available.
func peerCredentials(nc net.Conn) (peerIdentity, error) {
	rc, ok := nc.(syscall.Conn)
	if !ok {
		return peerIdentity{}, errors.New("control peer is not a raw-capable unix connection")
	}
	raw, err := rc.SyscallConn()
	if err != nil {
		return peerIdentity{}, err
	}
	var (
		cred *unix.Xucred
		gerr error // getsockopt's own failure, reported through Control
	)
	const (
		solLocal      = 0x0 // option level for non-SOL_SOCKET local options
		localPeerCred = 0x1 // LOCAL_PEERCRED: the peer's struct xucred
	)
	if cerr := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), solLocal, localPeerCred)
	}); cerr != nil {
		return peerIdentity{}, cerr
	} else if gerr != nil {
		return peerIdentity{}, gerr
	}
	if cred == nil {
		return peerIdentity{}, errors.New("LOCAL_PEERCRED returned no credentials")
	}
	return peerIdentity{UID: int(cred.Uid)}, nil
}
