//go:build linux

package runtimeipc

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"

	"submux/internal/runtimeapi"
)

func platformPeerIdentity(connection *net.UnixConn) (runtimeapi.PeerIdentity, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("open Runtime Socket credentials: %w", err)
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("inspect Runtime Socket credentials: %w", err)
	}
	if socketErr != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read Runtime Socket credentials: %w", socketErr)
	}
	return runtimeapi.PeerIdentity{
		Platform: "linux",
		UID:      credentials.Uid,
		GID:      credentials.Gid,
		PID:      uint32(credentials.Pid),
	}, nil
}

func currentIdentity() (runtimeapi.PeerIdentity, error) {
	return runtimeapi.PeerIdentity{
		Platform: "linux",
		UID:      uint32(os.Getuid()),
		GID:      uint32(os.Getgid()),
		PID:      uint32(os.Getpid()),
	}, nil
}
