//go:build linux

package app

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerCredentials reads the kernel's credential snapshot for one accepted unix
// connection: effective uid and pid of the connecting process at connect time.
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
		cred *unix.Ucred
		gerr error // getsockopt's own failure, reported through Control
	)
	if cerr := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return peerIdentity{}, cerr
	} else if gerr != nil {
		return peerIdentity{}, gerr
	}
	if cred == nil {
		return peerIdentity{}, errors.New("SO_PEERCRED returned no credentials")
	}
	return peerIdentity{UID: int(cred.Uid), PID: int(cred.Pid)}, nil
}
