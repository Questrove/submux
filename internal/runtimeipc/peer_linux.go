//go:build linux

package runtimeipc

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"submux/internal/runtimeapi"
)

func platformPeerIdentity(connection *net.UnixConn) (runtimeapi.PeerIdentity, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("open Runtime Socket credentials: %w", err)
	}
	var credentials *unix.Ucred
	var groupIDs []uint32
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if socketErr == nil {
			groupIDs, socketErr = linuxPeerGroups(int(fd), credentials.Gid)
		}
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
		GroupIDs: groupIDs,
	}, nil
}

func currentIdentity() (runtimeapi.PeerIdentity, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return runtimeapi.PeerIdentity{}, fmt.Errorf("read current Runtime groups: %w", err)
	}
	groupIDs := make([]uint32, 0, len(groups)+1)
	groupIDs = append(groupIDs, uint32(os.Getgid()))
	for _, group := range groups {
		if group >= 0 && !containsGroupID(groupIDs, uint32(group)) {
			groupIDs = append(groupIDs, uint32(group))
		}
	}
	return runtimeapi.PeerIdentity{
		Platform: "linux",
		UID:      uint32(os.Getuid()),
		GID:      uint32(os.Getgid()),
		PID:      uint32(os.Getpid()),
		GroupIDs: groupIDs,
	}, nil
}

func linuxPeerGroups(fd int, primary uint32) ([]uint32, error) {
	const maximumGroupBytes = 256 << 10
	buffer := make([]byte, maximumGroupBytes)
	size := uint32(len(buffer))
	_, _, errno := unix.Syscall6(
		unix.SYS_GETSOCKOPT,
		uintptr(fd),
		uintptr(unix.SOL_SOCKET),
		uintptr(unix.SO_PEERGROUPS),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
	)
	if errno == unix.ENOPROTOOPT || errno == unix.EINVAL {
		return []uint32{primary}, nil
	}
	if errno != 0 {
		return nil, errno
	}
	if size == 0 || size > uint32(len(buffer)) || size%4 != 0 {
		return nil, fmt.Errorf("Runtime Socket peer group credentials have invalid length %d", size)
	}
	groupIDs := make([]uint32, 0, int(size/4)+1)
	groupIDs = append(groupIDs, primary)
	for offset := uint32(0); offset < size; offset += 4 {
		group := binary.NativeEndian.Uint32(buffer[offset : offset+4])
		if !containsGroupID(groupIDs, group) {
			groupIDs = append(groupIDs, group)
		}
	}
	return groupIDs, nil
}
