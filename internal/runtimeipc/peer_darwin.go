//go:build darwin

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
	var credentials *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("inspect Runtime Socket credentials: %w", err)
	}
	if socketErr != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read Runtime Socket credentials: %w", socketErr)
	}
	var gid uint32
	if credentials.Ngroups > 0 {
		gid = credentials.Groups[0]
	}
	return runtimeapi.PeerIdentity{
		Platform: "darwin",
		UID:      credentials.Uid,
		GID:      gid,
	}, nil
}

func currentIdentity() (runtimeapi.PeerIdentity, error) {
	return runtimeapi.PeerIdentity{
		Platform: "darwin",
		UID:      uint32(os.Getuid()),
		GID:      uint32(os.Getgid()),
		PID:      uint32(os.Getpid()),
	}, nil
}
